package weft

// dispatch.go is the multi-host driver lookup layer. For each
// Host registered in the cluster, the control plane carries a
// HostHandle — a tuple of (HypervisorDriver, NetworkDriver,
// VolumeDriver, ImageDriver) addressed by the Host's UUID.
//
// In single-host installs (today's only deployment shape) the
// dispatch table has one entry: the locally-built Bundle
// from `vzd-driver-apple-vz/builtin`, registered under the
// self-registered host's UUID. When vzd-agent lands and remote
// hosts come online, each agent's drivers register here as
// `*grpc.RemoteHypervisor` etc. — same dispatch table, same
// lookup, no call-site changes.
//
// The interface-typed fields are what makes the same dispatch
// path work for both local + remote drivers: vzd-driver-apple-vz's
// *Hypervisor satisfies drivers.HypervisorDriver, and a future
// gRPC client stub will too. Per [[vzd-driver-registry-split]],
// callers depend on the *interface*, not the concrete type.

import (
	"fmt"

	drivers "github.com/openweft/weft-drivers"
)

// HostHandle bundles the four driver instances vzd-control
// addresses one compute host through. Each field is an
// interface — concrete implementations come from
// `vzd-driver-apple-vz/builtin` (local), a future gRPC client
// module (remote), or hand-rolled fakes (tests).
type HostHandle struct {
	Hypervisor drivers.HypervisorDriver
	Network    drivers.NetworkDriver
	Volume     drivers.VolumeDriver
	Image      drivers.ImageDriver
}

// RegisterHostHandle adds (or replaces) the dispatch entry for
// hostUUID. Used by vzd-agent's bootstrap to register itself,
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

// UnregisterHostHandle removes the dispatch entry. Used by
// vzd-agent disconnect handling + by tests for symmetry with
// RegisterHostHandle.
func (a *Adapter) UnregisterHostHandle(hostUUID string) {
	a.driverDispatchMu.Lock()
	defer a.driverDispatchMu.Unlock()
	delete(a.driverDispatch, hostUUID)
}

// HostHandleOn returns the dispatch entry for hostUUID. Error
// when the host has no registered handle (either it was never
// registered, or the remote agent disconnected).
func (a *Adapter) HostHandleOn(hostUUID string) (*HostHandle, error) {
	a.driverDispatchMu.RLock()
	defer a.driverDispatchMu.RUnlock()
	h, ok := a.driverDispatch[hostUUID]
	if !ok {
		return nil, fmt.Errorf("host %q has no driver handle registered", hostUUID)
	}
	return h, nil
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

// hypervisorForVM resolves the HypervisorDriver responsible for
// the named VM by walking through the VM inventory:
//
//  1. findVMByName(name) gives the project the VM lives under
//     (filesystem scan — same source of truth lifecycle methods
//     use for the vmDir path).
//  2. vmRegistry.lookupByName(project, name) gives the VM
//     record + its host_uuid.
//  3. HypervisorOn(host_uuid) returns the driver — local for
//     single-host installs, remote (over gRPC) once vzd-agent
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
		if project, _, ok := a.findVMByName(name); ok {
			if vm, ok := a.vmReg.lookupByName(project, name); ok && vm.HostUUID != "" {
				return a.HypervisorOn(vm.HostUUID)
			}
		}
	}
	return a.localHypervisor()
}
