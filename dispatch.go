package weft

// dispatch.go is the multi-host driver lookup layer. For each
// Host registered in the cluster, the control plane carries a
// HostHandle — a tuple of (HypervisorDriver, NetworkDriver,
// VolumeDriver, ImageDriver) addressed by the Host's UUID.
//
// In single-host installs (today's only deployment shape) the
// dispatch table has one entry: the locally-built Bundle
// from `weft-driver-vz/builtin`, registered under the
// self-registered host's UUID. When weft-agent lands and remote
// hosts come online, each agent's drivers register here as
// `*grpc.RemoteHypervisor` etc. — same dispatch table, same
// lookup, no call-site changes.
//
// The interface-typed fields are what makes the same dispatch
// path work for both local + remote drivers: weft-driver-vz's
// *Hypervisor satisfies drivers.HypervisorDriver, and a future
// gRPC client stub will too. Per [[weft-driver-registry-split]],
// callers depend on the *interface*, not the concrete type.

import (
	"fmt"

	drivers "github.com/openweft/weft-drivers"
)

// HostHandle bundles the four driver instances weft-control
// addresses one compute host through. Each field is an
// interface — concrete implementations come from
// `weft-driver-vz/builtin` (local), a future gRPC client
// module (remote), or hand-rolled fakes (tests).
type HostHandle struct {
	Hypervisor drivers.HypervisorDriver
	Network    drivers.NetworkDriver
	Volume     drivers.VolumeDriver
	Image      drivers.ImageDriver
}

// RegisterHostHandle adds (or replaces) the dispatch entry for
// hostUUID. Used by weft-agent's bootstrap to register itself,
// and by tests that want to install fake drivers.
//
// Returns an error when hostUUID is empty or handle nil — the
// caller almost always has a bug if either condition is true.
func (a *Adapter) RegisterHostHandle(hostUUID string, handle *HostHandle) error {
	if hostUUID == "" {
		return fmt.Errorf("RegisterHostHandle: empty hostUUID")
	}
	if handle == nil {
		return fmt.Errorf("RegisterHostHandle: nil handle")
	}
	a.driverDispatchMu.Lock()
	defer a.driverDispatchMu.Unlock()
	if a.driverDispatch == nil {
		a.driverDispatch = make(map[string]*HostHandle)
	}
	a.driverDispatch[hostUUID] = handle
	return nil
}

// RegisterHostHandleSet installs the multi-driver dispatch for a
// host. Used by weft-agents in multi-plugin mode (Apple Silicon
// running both weft-driver-vz + weft-driver-qemu side-by-side) —
// each kind ("vz" / "qemu") points at its own HostHandle.
//
// Single-driver hosts keep using RegisterHostHandle ; the per-kind
// table is consulted FIRST on lookups, and falls back to the
// single-entry table when no set is registered. The "primary" kind
// (vz before qemu in deterministic order) is also written into the
// single-entry table so call sites that don't know the VM's arch
// keep working transitionally.
func (a *Adapter) RegisterHostHandleSet(hostUUID string, set map[string]*HostHandle) error {
	if hostUUID == "" {
		return fmt.Errorf("RegisterHostHandleSet: empty hostUUID")
	}
	if len(set) == 0 {
		return fmt.Errorf("RegisterHostHandleSet: empty set")
	}
	for kind, h := range set {
		if h == nil {
			return fmt.Errorf("RegisterHostHandleSet: nil handle for kind %q", kind)
		}
	}
	a.driverDispatchMu.Lock()
	defer a.driverDispatchMu.Unlock()
	if a.driverDispatchSet == nil {
		a.driverDispatchSet = make(map[string]map[string]*HostHandle)
	}
	// Defensive copy — caller may mutate their slice / map after the call.
	stored := make(map[string]*HostHandle, len(set))
	for k, v := range set {
		stored[k] = v
	}
	a.driverDispatchSet[hostUUID] = stored
	// Mirror the PRIMARY (vz > qemu) into the single-entry table so
	// HostHandleOn (which doesn't know about arch) keeps returning a
	// usable handle until every call site has migrated to
	// HostHandleOnArch.
	if a.driverDispatch == nil {
		a.driverDispatch = make(map[string]*HostHandle)
	}
	// vz/qemu win when present (hardware virt) ; wasm last as the
	// fallback backend for hosts with no virt extensions.
	for _, kind := range []string{"vz", "qemu", "wasm"} {
		if h, ok := stored[kind]; ok {
			a.driverDispatch[hostUUID] = h
			break
		}
	}
	return nil
}

// UnregisterHostHandle removes the dispatch entry. Used by
// weft-agent disconnect handling + by tests for symmetry with
// RegisterHostHandle. Clears BOTH the single-entry table and the
// per-kind set (the host is offline whichever mode it ran in).
func (a *Adapter) UnregisterHostHandle(hostUUID string) {
	a.driverDispatchMu.Lock()
	defer a.driverDispatchMu.Unlock()
	delete(a.driverDispatch, hostUUID)
	delete(a.driverDispatchSet, hostUUID)
}

// HostHandleOn returns the dispatch entry for hostUUID. Error
// when the host has no registered handle (either it was never
// registered, or the remote agent disconnected).
//
// In multi-plugin mode this returns the PRIMARY entry (the kind
// chosen by deterministic ordering on RegisterHostHandleSet, today
// vz before qemu). For an arch-aware lookup use HostHandleOnArch.
func (a *Adapter) HostHandleOn(hostUUID string) (*HostHandle, error) {
	a.driverDispatchMu.RLock()
	defer a.driverDispatchMu.RUnlock()
	h, ok := a.driverDispatch[hostUUID]
	if !ok {
		return nil, fmt.Errorf("host %q has no driver handle registered", hostUUID)
	}
	return h, nil
}

// HostHandleOnArch returns the dispatch entry for hostUUID with the
// driver kind that covers the given guest arch. Used by VM-lifecycle
// paths that route a workload to the right driver on a multi-plugin
// host (Apple Silicon : arm64 → vz, amd64 → qemu).
//
// Resolution order :
//  1. If the host has a multi-plugin set registered, consult the
//     host registry's Drivers capability list to find which kind
//     covers `arch`. Return that kind's entry.
//  2. If no multi-plugin set, fall back to HostHandleOn (which
//     returns the host's single driver). Single-plugin hosts that
//     happen to support `arch` work transitionally.
//  3. Missing arch coverage on a multi-plugin host returns an error
//     ("host has no driver covering arch X") — the scheduler should
//     have caught this before dispatch, so reaching it here is a bug.
//
// Empty `arch` falls back to HostHandleOn — the caller doesn't have
// an arch constraint, return whatever the primary is.
func (a *Adapter) HostHandleOnArch(hostUUID, arch string) (*HostHandle, error) {
	a.driverDispatchMu.RLock()
	set, hasSet := a.driverDispatchSet[hostUUID]
	a.driverDispatchMu.RUnlock()
	if !hasSet || arch == "" {
		return a.HostHandleOn(hostUUID)
	}
	if a.hostReg == nil {
		// Without a host registry we can't map arch → kind ; fall
		// back to the primary single-driver entry rather than failing.
		return a.HostHandleOn(hostUUID)
	}
	host, ok := a.hostReg.lookupByUUID(hostUUID)
	if !ok {
		return a.HostHandleOn(hostUUID)
	}
	for _, d := range host.Drivers {
		for _, a := range d.Arches {
			if a == arch {
				if h, ok := set[d.Kind]; ok {
					return h, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("host %q has no driver covering arch %q", hostUUID, arch)
}

// HypervisorOn returns the HypervisorDriver for hostUUID. Most
// VM-lifecycle paths use this directly rather than going through
// HostHandleOn — keeps the call site one line.
func (a *Adapter) HypervisorOn(hostUUID string) (drivers.HypervisorDriver, error) {
	h, err := a.HostHandleOn(hostUUID)
	if err != nil {
		return nil, err
	}
	return h.Hypervisor, nil
}

// NetworkOn / VolumeOn / ImageOn mirror HypervisorOn for the
// other three driver types. Same dispatch table, same lookup
// semantics.
func (a *Adapter) NetworkOn(hostUUID string) (drivers.NetworkDriver, error) {
	h, err := a.HostHandleOn(hostUUID)
	if err != nil {
		return nil, err
	}
	return h.Network, nil
}

func (a *Adapter) VolumeOn(hostUUID string) (drivers.VolumeDriver, error) {
	h, err := a.HostHandleOn(hostUUID)
	if err != nil {
		return nil, err
	}
	return h.Volume, nil
}

func (a *Adapter) ImageOn(hostUUID string) (drivers.ImageDriver, error) {
	h, err := a.HostHandleOn(hostUUID)
	if err != nil {
		return nil, err
	}
	return h.Image, nil
}

// DispatchedHosts returns the set of host UUIDs that have a
// registered handle. Useful for diagnostics + as input to the
// scheduler when ScheduleRequest's other constraints would be
// trivially satisfied.
func (a *Adapter) DispatchedHosts() []string {
	a.driverDispatchMu.RLock()
	defer a.driverDispatchMu.RUnlock()
	out := make([]string, 0, len(a.driverDispatch))
	for u := range a.driverDispatch {
		out = append(out, u)
	}
	return out
}

// localHostUUID returns the UUID under which the local Bundle
// is registered. Today's single-host lifecycle methods
// (DeleteVM, StopVM, StartVM, …) use this to resolve "which
// host's driver to invoke" — until VM-inventory carries a
// `host_uuid` field, the answer is always "this host".
func (a *Adapter) localHostUUID() string {
	u, _ := a.loadOrCreateHostUUID()
	return u
}

// localHypervisor is the convenience that today's call sites
// use: "give me the local host's HypervisorDriver". Wraps
// HypervisorOn(localHostUUID) so failures get a useful message
// without the call site needing to know.
func (a *Adapter) localHypervisor() (drivers.HypervisorDriver, error) {
	uuid := a.localHostUUID()
	if uuid == "" {
		return nil, fmt.Errorf("local host UUID unavailable (host-uuid file unreadable?)")
	}
	return a.HypervisorOn(uuid)
}

// LookupKind reports which driver kind the dispatch tables would pick
// for (hostUUID, arch). Mirrors the resolution order of HostHandleOnArch
// without performing the handle lookup — useful for observability seams
// (e.g. tagging a Prometheus counter with `driver_kind="vz"` so per-driver
// error rates can be alerted on independently).
//
// Return values :
//
//   - On a multi-plugin host (driverDispatchSet populated) — the kind
//     covering `arch` per the host registry's Drivers capability list
//     ("vz" / "qemu" / future siblings), or "" if no driver covers arch.
//   - On a single-plugin host — the host's legacy Hypervisor label
//     ("apple-vz" / "qemu" / etc.) when registered, otherwise "".
//   - On an unknown host (or empty hostUUID) — "".
//
// The interceptor / metric layer treats "" as "this RPC did not route
// through a driver" (host-registry / scheduling-rule / etc.) so a single
// `driver_kind=""` time-series captures the non-routed traffic without
// pretending it's part of any kind's rate.
func (a *Adapter) LookupKind(hostUUID, arch string) string {
	if hostUUID == "" {
		return ""
	}
	a.driverDispatchMu.RLock()
	set, hasSet := a.driverDispatchSet[hostUUID]
	_, hasSingle := a.driverDispatch[hostUUID]
	a.driverDispatchMu.RUnlock()
	if hasSet {
		// Multi-plugin host. When arch is empty fall back to the
		// PRIMARY (vz before qemu in stable order) — same rule
		// HostHandleOnArch uses, so the label matches the handle.
		if arch == "" {
			for _, k := range []string{"vz", "qemu"} {
				if _, ok := set[k]; ok {
					return k
				}
			}
			// Defensive : non-canonical kinds (future siblings) — return any.
			for k := range set {
				return k
			}
			return ""
		}
		if a.hostReg != nil {
			if host, ok := a.hostReg.lookupByUUID(hostUUID); ok {
				for _, d := range host.Drivers {
					for _, ha := range d.Arches {
						if ha == arch {
							if _, ok := set[d.Kind]; ok {
								return d.Kind
							}
						}
					}
				}
			}
		}
		return ""
	}
	if !hasSingle {
		return ""
	}
	// Single-plugin host. Surface the host registry's `Hypervisor`
	// label so operators see "apple-vz" / "qemu" — matches the legacy
	// hypervisor-label semantics on the Host registration.
	if a.hostReg != nil {
		if host, ok := a.hostReg.lookupByUUID(hostUUID); ok {
			return host.Hypervisor
		}
	}
	// Host registry not wired (early-boot or test fixture) — non-empty
	// label "single" so the metric tracks that the RPC routed through
	// a driver, without inventing a name.
	return ""
}

// LookupKindForVM is the convenience wrapper that resolves a VM by
// display name + its arch off the inventory record, then asks
// LookupKind. Empty when the VM isn't in the inventory (legacy on-disk
// VM provisioned before vmRegistry landed) — caller treats that the
// same as a non-driver-routed RPC.
func (a *Adapter) LookupKindForVM(name string) string {
	if a.vmReg == nil || name == "" {
		return ""
	}
	project, _, ok := a.findVMByName(name)
	if !ok {
		return ""
	}
	vm, ok := a.vmReg.lookupByName(project, name)
	if !ok || vm.HostUUID == "" {
		return ""
	}
	return a.LookupKind(vm.HostUUID, vm.Architecture)
}

// hypervisorForVM resolves the HypervisorDriver responsible for
// the named VM by walking through the VM inventory:
//
//  1. findVMByName(name) gives the project the VM lives under
//     (filesystem scan — same source of truth lifecycle methods
//     use for the vmDir path).
//  2. vmRegistry.lookupByName(project, name) gives the VM
//     record + its host_uuid.
//  3. HypervisorOn(host_uuid) returns the driver — local for
//     single-host installs, remote (over gRPC) once weft-agent
//     lands.
//
// Falls back to the local hypervisor when:
//
//   - the VM isn't in the inventory (legacy on-disk VM that was
//     never RegisterVM'd — covers everything provisioned before
//     this integration step landed),
//   - the inventory entry has an empty host_uuid (defensive).
//
// The fallback is what makes this integration backwards-
// compatible: existing VMs keep working as if nothing changed,
// new VMs go through the registry.
func (a *Adapter) hypervisorForVM(name string) (drivers.HypervisorDriver, error) {
	if a.vmReg != nil {
		// When several projects carry a VM record with the same name
		// (the operator ran `weft microvm run` in multiple project
		// namespaces, mostly during day-0 bring-up), the
		// filesystem-scan picks one arbitrarily. Prefer a record
		// pinned to LocalHostUUID() so the dispatch lands on the
		// local driver bundle — avoids the "VM record exists but on
		// another host" failure mode the respawn loop hits.
		local := a.LocalHostUUID()
		if local != "" {
			a.vmReg.mu.Lock()
			var pick VM
			for _, v := range a.vmReg.byUUID {
				if v.Name == name && v.HostUUID == local {
					pick = v
					break
				}
			}
			a.vmReg.mu.Unlock()
			if pick.UUID != "" {
				handle, err := a.HostHandleOnArch(pick.HostUUID, pick.Architecture)
				if err != nil {
					return nil, err
				}
				return handle.Hypervisor, nil
			}
		}
		if project, _, ok := a.findVMByName(name); ok {
			if vm, ok := a.vmReg.lookupByName(project, name); ok && vm.HostUUID != "" {
				// VM.Architecture, when set, drives the per-kind
				// dispatch on multi-plugin hosts (Apple Silicon dual
				// VZ + QEMU). When empty, HostHandleOnArch falls
				// back to the primary handle — same behaviour as
				// before this commit for legacy single-driver VMs.
				handle, err := a.HostHandleOnArch(vm.HostUUID, vm.Architecture)
				if err != nil {
					return nil, err
				}
				return handle.Hypervisor, nil
			}
		}
	}
	return a.localHypervisor()
}
