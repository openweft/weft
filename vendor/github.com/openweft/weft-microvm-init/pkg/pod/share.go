package pod

import (
	"encoding/json"
	"fmt"
	"os"
)

// MountAction distinguishes a mount request from an unmount on the event
// bus. The zero value ("") means mount, so a boot-time Spec.ShareMounts
// entry needs no explicit action.
type MountAction string

const (
	MountActionMount   MountAction = "" // also "mount"
	MountActionUnmount MountAction = "unmount"
)

// BackendCubeFS is the platform's RWX share backend — CubeFS, a CNCF
// distributed POSIX + S3 filesystem. The Backend field is kept so other
// backends can return without reshaping the event-bus contract.
const BackendCubeFS = "cubefs"

// ShareMount is one multi-attach POSIX share mounted inside the guest. It
// serves two roles, mirroring how pod.WireGuard is both a Spec field and
// the mesh update message:
//
//   - boot-time: an entry in Spec.ShareMounts the agent mounts at startup;
//   - dynamic: the JSON payload an operator (e.g. a teacher) publishes on
//     the event bus to mount/unmount a share on a running VM — which the
//     control plane fans out to a whole group of student VMs.
//
// State is pushed whole and applied idempotently (replace-by-ID), so a
// missed message self-heals on the next publish — same model as the mesh.
type ShareMount struct {
	// ID is a stable handle, required so a later unmount (or re-publish)
	// targets the same mount deterministically.
	ID string `json:"id"`

	// Action is "" / "mount" or "unmount". Unmount only needs ID + MountPoint.
	Action MountAction `json:"action,omitempty"`

	// Backend selects the storage backend. Empty defaults to CubeFS.
	Backend string `json:"backend,omitempty"`

	// MountPoint is the absolute guest path the share appears at, e.g.
	// "/run/weft/shares/team-data". Bind-mount it into containers via
	// Container.Mounts to expose it to a workload.
	MountPoint string `json:"mount_point"`

	// Readonly mounts the share read-only.
	Readonly bool `json:"readonly,omitempty"`

	// CubeFS carries the CubeFS connection details (required when Backend
	// is CubeFS / empty).
	CubeFS *CubeFSMount `json:"cubefs,omitempty"`
}

// CubeFSMount is the connection detail for a CubeFS volume — what the
// in-guest CubeFS client (cfs-client) needs to attach the volume.
type CubeFSMount struct {
	// Volume is the CubeFS volume name (the platform "share" maps 1:1 to
	// a CubeFS volume).
	Volume string `json:"volume"`

	// Masters are the CubeFS master node addresses ("host:port"), reached
	// over the wg0 overlay.
	Masters []string `json:"masters"`

	// Owner is the volume owner CubeFS authorizes against.
	Owner string `json:"owner"`

	// AccessKey / SecretKey authenticate to CubeFS (the volume's keypair).
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`

	// SubDir optionally mounts only a subtree of the volume.
	SubDir string `json:"subdir,omitempty"`
}

func (m *ShareMount) backend() string {
	if m.Backend == "" {
		return BackendCubeFS
	}
	return m.Backend
}

// Validate checks the fields the action+backend need. Unmount only needs
// ID + MountPoint ; a CubeFS mount needs the volume connection set.
func (m *ShareMount) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("id is required")
	}
	if m.Action == MountActionUnmount {
		if m.MountPoint == "" {
			return fmt.Errorf("unmount %q: mount_point is required", m.ID)
		}
		return nil
	}
	if m.MountPoint == "" {
		return fmt.Errorf("mount %q: mount_point is required", m.ID)
	}
	switch m.backend() {
	case BackendCubeFS:
		if m.CubeFS == nil {
			return fmt.Errorf("mount %q: cubefs block is required", m.ID)
		}
		if m.CubeFS.Volume == "" {
			return fmt.Errorf("mount %q: cubefs.volume is required", m.ID)
		}
		if len(m.CubeFS.Masters) == 0 {
			return fmt.Errorf("mount %q: cubefs.masters is required", m.ID)
		}
		if m.CubeFS.Owner == "" {
			return fmt.Errorf("mount %q: cubefs.owner is required", m.ID)
		}
	default:
		return fmt.Errorf("mount %q: unsupported backend %q", m.ID, m.Backend)
	}
	return nil
}

// EffectiveBackend reports the backend, applying the CubeFS default.
func (m *ShareMount) EffectiveBackend() string { return m.backend() }

// LoadShareMounts reads a standalone list of share mounts (JSON array)
// from path — the file the host drops into the config share for boot-time
// mounts. A missing file is not an error (returns nil, nil).
func LoadShareMounts(path string) ([]ShareMount, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var mounts []ShareMount
	if err := json.Unmarshal(b, &mounts); err != nil {
		return nil, fmt.Errorf("share mounts %s: %w", path, err)
	}
	for i := range mounts {
		if err := mounts[i].Validate(); err != nil {
			return nil, fmt.Errorf("share mounts %s: %w", path, err)
		}
	}
	return mounts, nil
}
