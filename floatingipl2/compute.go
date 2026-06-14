package floatingipl2

import (
	weft "github.com/openweft/weft"
)

// Scope is the read-only adapter view ComputeLocalL2Mappings
// needs : every method the production *weft.Adapter already
// implements. Kept narrow so tests inject a hand-rolled stub
// without dragging the full adapter surface.
type Scope interface {
	ListVMsForHost(hostUUID string) []weft.VM
	ListFloatingIPs() []weft.FloatingIP
	NetworkByUUID(uuid string) (weft.Network, bool)
}

// ComputeLocalL2Mappings is the pure projection from
// "adapter snapshot + host UUID" → "L2 mappings for THIS host".
// Walks every FIP, drops the ones not mapped to a local VM,
// drops the ones whose network is not VLAN-mode, returns one
// L2Mapping per surviving FIP.
//
// Symmetric to floatingipnat.ComputeLocalMappings — same
// "is local VM" gate, different output shape (no need for the
// private IP, the macvlan binds the public IP directly).
//
// VMs without an IP yet (still booting) ARE included here :
// the macvlan + ARP path doesn't depend on the VM having a
// port-assigned private IP, only on the host running the VM.
// (The NAT reconciler — floatingipnat — does need the private
// IP, but that's its problem.)
func ComputeLocalL2Mappings(scope Scope, hostUUID string) []L2Mapping {
	localVMs := make(map[string]weft.VM)
	for _, vm := range scope.ListVMsForHost(hostUUID) {
		localVMs[vm.Name] = vm
	}
	if len(localVMs) == 0 {
		return nil
	}

	var out []L2Mapping
	for _, fip := range scope.ListFloatingIPs() {
		if fip.Status != weft.FIPStatusActive || fip.TargetKind != weft.FIPTargetVM {
			continue
		}
		vm, local := localVMs[fip.MappedTo]
		if !local {
			continue
		}
		net, ok := scope.NetworkByUUID(fip.NetworkUUID)
		if !ok {
			continue
		}
		if net.ExternalMode != weft.NetworkExternalVLAN {
			continue
		}
		out = append(out, L2Mapping{
			PublicIP:        fip.Address,
			NetworkUUID:     fip.NetworkUUID,
			VLAN:            net.VLAN,
			ParentInterface: net.ParentInterface,
			VMName:          vm.Name,
		})
	}
	return out
}
