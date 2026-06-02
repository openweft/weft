package weft

// adapter_firewallpub.go is the narrow extension the firewallpub
// package needs on top of the standard Adapter surface : a sorted
// snapshot of every port in the registry.
//
// Kept in a sibling file (rather than in adapter.go) so it's clear
// the method exists for one consumer and so a future SG-indexed
// lookup can land here without churning the rest of the adapter.

// ListAllPorts returns a copy of every port in the registry, sorted
// by (ProjectUUID, NetworkUUID, IP). The firewallpub publisher uses
// it to scan for VMs impacted by a Security-Group rule change. For
// small / medium fleets the full scan is cheap ; for very large
// deployments the publisher can be re-pointed at a SG-indexed
// lookup landed here later.
func (a *Adapter) ListAllPorts() []Port {
	if a.portReg == nil {
		return nil
	}
	return a.portReg.list()
}
