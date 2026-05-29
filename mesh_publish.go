package weft

import (
	"net/netip"

	"github.com/openweft/weft/mesh"
)

// PublishMesh recomputes every member's wg0 config from the given mesh
// membership and fans it out on each VM's mesh subject — the daemon-side
// symmetric counterpart of AttachShareToProject for shares. Returns the
// number of VMs the new mesh state was published to.
//
// PublishMesh takes already-resolved members (key, overlay IP, endpoint)
// because the per-VM WG state lives outside the VM registry today; vzd is
// expected to call this on lifecycle events (provision / delete / move)
// once the WG state plumbing lands. No CLI for now: mesh updates are
// derived state, not operator-supplied data — contrast with share attach
// where the operator naturally supplies the CubeFS connection details.
func (a *Adapter) PublishMesh(members []mesh.Member, subnet netip.Prefix, keepalive uint16) (int, error) {
	nc, err := natsConnFromBus(a.bus)
	if err != nil {
		return 0, err
	}
	cfgs, err := mesh.Build(subnet, members, keepalive)
	if err != nil {
		return 0, err
	}
	if err := mesh.PublishAll(nc, cfgs); err != nil {
		return 0, err
	}
	return len(cfgs), nil
}
