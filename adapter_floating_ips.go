package weft

// adapter_floating_ips.go is the public Adapter surface for the
// floating-IP registry plus the init shim called from
// NewWithStorage. Kept in a sibling file (not in adapter.go) so
// the change stays narrow + reviewable, mirroring
// adapter_firewallpub.go's pattern.
//
// Every mutation here publishes a "floating_ip.<verb>" platform
// event so the future host-side NAT reconciler (and the webui
// activity feed) can react without polling. Validation : the
// caller is responsible for sg-style proj/network existence
// checks at the gRPC layer — the registry only enforces address
// uniqueness, lifecycle invariants, and CIDR coherence.

import (
	"context"
	"fmt"
	"os"
)

// initFloatingIPs loads the on-disk floating-IP registry via
// storageFactory. Failure to load downgrades to an empty in-memory
// registry — same resilience contract as the other registries.
func (a *Adapter) initFloatingIPs() {
	storage := a.storageFactory("floating_ips")
	reg, err := loadFloatingIPRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load floating-ip registry: %v\n", err)
		reg = newFloatingIPRegistry(storage)
	}
	a.fipReg = reg
}

// ListFloatingIPs returns every FIP across all projects, sorted
// by (ProjectUUID, Address). Operator-facing — the per-project
// ListFloatingIPsForProject narrows for tenant views.
func (a *Adapter) ListFloatingIPs() []FloatingIP {
	if a.fipReg == nil {
		return nil
	}
	return a.fipReg.list()
}

// ListFloatingIPsForProject narrows to one tenant.
func (a *Adapter) ListFloatingIPsForProject(projectUUID string) []FloatingIP {
	if a.fipReg == nil {
		return nil
	}
	return a.fipReg.listForProject(projectUUID)
}

// FloatingIPByUUID is the lookup path used by the Map/Unmap
// handlers (project authorization happens at the gRPC layer).
func (a *Adapter) FloatingIPByUUID(uuid string) (FloatingIP, bool) {
	if a.fipReg == nil {
		return FloatingIP{}, false
	}
	return a.fipReg.lookupByUUID(uuid)
}

// ListFloatingIPsForTarget returns every FIP currently mapped to
// (kind, name) — the query the future host-side NAT reconciler
// will use to figure out which addresses to DNAT to a given VM.
func (a *Adapter) ListFloatingIPsForTarget(kind FloatingIPTargetKind, target string) []FloatingIP {
	if a.fipReg == nil {
		return nil
	}
	return a.fipReg.listForTarget(kind, target)
}

// AllocateFloatingIP picks (or validates) a free address on
// networkUUID and persists a new "available" FloatingIP. The
// project and network must exist ; mismatched (network not in
// project) is refused. Address selection skips port-occupied
// IPs and the network's reserved gateway/DHCP IPs.
//
// emits : floating_ip.allocated { uuid, project, network, address }
func (a *Adapter) AllocateFloatingIP(projectUUID, networkUUID, address string) (FloatingIP, error) {
	if a.fipReg == nil {
		return FloatingIP{}, fmt.Errorf("floating-ip registry not initialised")
	}
	n, ok := a.NetworkByUUID(networkUUID)
	if !ok {
		return FloatingIP{}, fmt.Errorf("network %q not found", networkUUID)
	}
	if n.ProjectUUID != projectUUID {
		return FloatingIP{}, fmt.Errorf("network %q belongs to project %s, not %s — cross-project reference refused",
			networkUUID, n.ProjectUUID, projectUUID)
	}
	if n.CIDR == "" {
		return FloatingIP{}, fmt.Errorf("network %q has no CIDR — cannot allocate floating IP", networkUUID)
	}

	// Gather every port-occupied address on this network so we
	// never hand out a private-IP collision. Same pattern Port
	// creation uses for its own uniqueness checks ([[ports]]).
	portsInUse := portIPsOnNetwork(a, networkUUID)
	reserved := networkReservedAddresses(n)

	fip, err := a.fipReg.allocate(AllocateFloatingIPSpec{
		ProjectUUID: projectUUID,
		NetworkUUID: networkUUID,
		Address:     address,
		PortInUse:   portsInUse,
		Reserved:    reserved,
	}, n.CIDR)
	if err != nil {
		return FloatingIP{}, err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "floating_ip.allocated",
		Subject:     fip.UUID,
		ProjectUUID: fip.ProjectUUID,
		Meta: map[string]string{
			"network_uuid": fip.NetworkUUID,
			"address":      fip.Address,
		},
	})
	return fip, nil
}

// ReleaseFloatingIP removes uuid from the pool. The FIP must be
// in "available" state — Map → Unmap → Release is the lifecycle
// the gRPC layer mirrors back to the caller as a precondition
// failure when violated.
//
// emits : floating_ip.released { uuid, project, address }
func (a *Adapter) ReleaseFloatingIP(uuid string) error {
	if a.fipReg == nil {
		return fmt.Errorf("floating-ip registry not initialised")
	}
	fip, err := a.fipReg.release(uuid)
	if err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "floating_ip.released",
		Subject:     fip.UUID,
		ProjectUUID: fip.ProjectUUID,
		Meta: map[string]string{
			"network_uuid": fip.NetworkUUID,
			"address":      fip.Address,
		},
	})
	return nil
}

// MapFloatingIP binds uuid to a target. Idempotent on the same
// (kind, target) pair ; refuses to overwrite a different mapping
// without an explicit Unmap first (the caller must make the
// intent visible — silent rebinding would mask config drift).
//
// emits : floating_ip.mapped { uuid, project, address, target_kind, target }
//
// rateLimitPPS : 0 = no limit on fresh map, leave existing on
// idempotent path. > 0 = anti-DDoS cap installed by the host-
// side reconciler (passes through to NATMapping.RateLimitPPS).
// Capped at 100_000 — anything higher rounds down.
func (a *Adapter) MapFloatingIP(uuid string, kind FloatingIPTargetKind, target string, rateLimitPPS int) (FloatingIP, error) {
	if a.fipReg == nil {
		return FloatingIP{}, fmt.Errorf("floating-ip registry not initialised")
	}
	if target == "" {
		return FloatingIP{}, fmt.Errorf("target name is required")
	}
	switch kind {
	case FIPTargetVM, FIPTargetLB:
	default:
		return FloatingIP{}, fmt.Errorf("unknown target_kind %q (want vm or lb)", kind)
	}
	if rateLimitPPS < 0 {
		return FloatingIP{}, fmt.Errorf("rate_limit_pps must be ≥ 0 : %d", rateLimitPPS)
	}
	if rateLimitPPS > 100_000 {
		rateLimitPPS = 100_000
	}
	fip, err := a.fipReg.mapTo(uuid, kind, target, rateLimitPPS)
	if err != nil {
		return FloatingIP{}, err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "floating_ip.mapped",
		Subject:     fip.UUID,
		ProjectUUID: fip.ProjectUUID,
		Meta: map[string]string{
			"network_uuid": fip.NetworkUUID,
			"address":      fip.Address,
			"target_kind":  string(fip.TargetKind),
			"target":       fip.MappedTo,
		},
	})
	return fip, nil
}

// UnmapFloatingIP clears the binding on uuid. Idempotent on
// already-unmapped — the FIP stays in the pool, the caller can
// Release it next.
//
// emits : floating_ip.unmapped { uuid, project, address }
func (a *Adapter) UnmapFloatingIP(uuid string) (FloatingIP, error) {
	if a.fipReg == nil {
		return FloatingIP{}, fmt.Errorf("floating-ip registry not initialised")
	}
	fip, err := a.fipReg.unmap(uuid)
	if err != nil {
		return FloatingIP{}, err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "floating_ip.unmapped",
		Subject:     fip.UUID,
		ProjectUUID: fip.ProjectUUID,
		Meta: map[string]string{
			"network_uuid": fip.NetworkUUID,
			"address":      fip.Address,
		},
	})
	return fip, nil
}

// portIPsOnNetwork returns every port-occupied address on
// networkUUID. Used by AllocateFloatingIP's exclusion list so a
// FIP can never collide with a private-port address.
func portIPsOnNetwork(a *Adapter, networkUUID string) []string {
	ports := a.ListPortsForNetwork(networkUUID)
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		if p.IP != "" {
			out = append(out, p.IP)
		}
	}
	return out
}

// networkReservedAddresses returns the addresses a Network
// reserves internally — today that's just the Gateway IP. The
// network address (.0) and broadcast (.255 on IPv4) are skipped
// by the registry's CIDR walk, no need to list them here.
func networkReservedAddresses(n Network) []string {
	if n.Gateway == "" {
		return nil
	}
	return []string{n.Gateway}
}
