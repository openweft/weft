package weft

import (
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/openweft/weft-microvm-init/pkg/pod"
	"github.com/openweft/weft/sharemount"
)

// AttachShareToProject fans a CubeFS share mount out to every VM in a
// project — the daemon-side core of `weft share attach`. It resolves the
// project's VMs from the inventory and publishes the same ShareMount to
// each VM's mount subject (weft.mounts.<vmID>); the in-VM agent applies it
// idempotently. The VM's UUID is the vmID, so the agent must be launched
// with -mounts-vm-id=<VM.UUID> (same convention the WireGuard mesh uses).
//
// m must carry the CubeFS connection details; it is validated before any
// publish (sharemount.PublishToGroup), so a malformed mount never reaches
// the bus.
// Returns the number of VMs the mount was fanned out to.
func (a *Adapter) AttachShareToProject(projectUUID string, m pod.ShareMount) (int, error) {
	nc, err := natsConnFromBus(a.bus)
	if err != nil {
		return 0, err
	}
	vms := a.ListVMsForProject(projectUUID)
	ids := make([]string, 0, len(vms))
	for _, v := range vms {
		ids = append(ids, v.UUID)
	}
	if err := sharemount.PublishToGroup(nc, ids, m); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// DetachShareFromProject is the inverse: publish an unmount for shareID at
// mountPoint to every VM in the project. Idempotent — VMs that never had
// the share simply no-op.
func (a *Adapter) DetachShareFromProject(projectUUID, shareID, mountPoint string) (int, error) {
	return a.AttachShareToProject(projectUUID, pod.ShareMount{
		ID:         shareID,
		Action:     pod.MountActionUnmount,
		MountPoint: mountPoint,
	})
}

// natsConnExposer is satisfied by NATSEventBus (Conn). The mount/mesh
// fan-out targets per-VM subjects outside the typed-event space, so it
// needs the raw connection.
type natsConnExposer interface{ Conn() *nats.Conn }

// natsConnFromBus extracts the raw NATS connection from the event bus, or
// errors when the bus isn't NATS-backed (e.g. an in-memory test bus) —
// share fan-out requires the real event bus.
func natsConnFromBus(bus EventBus) (*nats.Conn, error) {
	if e, ok := bus.(natsConnExposer); ok {
		if nc := e.Conn(); nc != nil {
			return nc, nil
		}
	}
	return nil, fmt.Errorf("share fan-out requires the NATS event bus")
}
