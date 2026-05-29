// Package sharemount is the control-plane side of dynamic share mounts. An
// operator (e.g. a teacher) attaches a CubeFS share to a group of VMs;
// weft publishes the same ShareMount on each VM's mount subject, and the
// in-VM agent (weft-vm-agent/pkg/mounts) subscribes and applies it.
//
// State is pushed whole and applied idempotently (replace-by-ID), so
// re-publishing or a missed message self-heals on the next publish — the
// same model the WireGuard mesh uses (see ../mesh).
package sharemount

import (
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

// Subject is the per-VM event-bus subject. Must match
// weft-vm-agent/pkg/mounts.Subject ("weft.mounts.<id>"). Publishing per-VM
// (rather than to a shared group subject) means a guest only ever trusts
// its own subject; the fan-out to a group is done here, host-side.
func Subject(vmID string) string { return "weft.mounts." + vmID }

// Publish sends one share mount/unmount to a single VM.
func Publish(nc *nats.Conn, vmID string, m pod.ShareMount) error {
	data, err := encode(m)
	if err != nil {
		return err
	}
	return nc.Publish(Subject(vmID), data)
}

// PublishToGroup fans the same mount out to every VM in a group — the call
// weft makes when a teacher attaches (or, with Action="unmount", detaches)
// a share to a class of student VMs. The payload is validated and marshalled
// once, then published to each member; every VM applies it independently and
// idempotently. Publishes to all members even if one send fails, returning
// the first error, and flushes so delivery is ordered before return.
//
// Resolving the group → VM ids (project membership, a class roster, a
// placement group) is the caller's job, using weft's inventory — mirroring
// how mesh.PublishAll takes an already-computed per-VM map.
func PublishToGroup(nc *nats.Conn, vmIDs []string, m pod.ShareMount) error {
	data, err := encode(m)
	if err != nil {
		return err
	}
	var first error
	for _, vmID := range vmIDs {
		if err := nc.Publish(Subject(vmID), data); err != nil && first == nil {
			first = fmt.Errorf("publish %s: %w", vmID, err)
		}
	}
	if ferr := nc.Flush(); ferr != nil && first == nil {
		first = ferr
	}
	return first
}

// encode validates then JSON-marshals a ShareMount. Validating here means a
// malformed mount never reaches the bus (and so never reaches any VM).
func encode(m pod.ShareMount) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("share mount %q: %w", m.ID, err)
	}
	return json.Marshal(m)
}
