package drivers

import "context"

// VolumeDriver materialises a Volume on its backing storage.
// One driver instance per backend: "file" (host-local qcow2/raw),
// "ceph" (cluster-wide RBD), "iscsi" (SAN), "nfs", …
//
// `Local()` is the key dispatch hint for the scheduler: a file-
// backed volume on host A can only attach to VMs that run on
// host A; a Ceph volume can attach anywhere.
//
// All methods are idempotent. EnsureVolume re-running with the
// same spec is a no-op; with a larger size it grows (matching
// the registry's grow-only contract).
type VolumeDriver interface {
	// Name identifies the backend, e.g. "file" / "ceph" / "iscsi".
	// Used in startup logs + the Host registry's VolumeBackends
	// capability list.
	Name() string

	// Local reports whether volumes from this driver are bound to
	// the driver's host (true) or accessible from any host (false).
	// File-backed → true; Ceph → false.
	Local() bool

	// HostInfo is only meaningful when Local() == true. Cluster-
	// wide drivers may return a synthetic HostInfo with UUID == ""
	// and Hostname == "<backend>-cluster".
	HostInfo(ctx context.Context) (HostInfo, error)

	// EnsureVolume creates (or grows) the backing storage.
	// Idempotent. Re-running with a smaller size is rejected;
	// the registry already enforces grow-only, but defence in
	// depth is cheap here.
	EnsureVolume(ctx context.Context, spec VolumeSpec) error

	// DestroyVolume removes the backing storage. No-op if missing.
	DestroyVolume(ctx context.Context, volumeUUID string) error

	// AttachVolume prepares the volume for hypervisor consumption
	// on the given host (e.g. maps an RBD device, locks the file,
	// stages a snapshot). Returns the BackingPath the hypervisor
	// opens.
	AttachVolume(ctx context.Context, volumeUUID, hostUUID string) (AttachedVolume, error)

	// DetachVolume releases the per-host attachment (unmap RBD,
	// release file lock). Does NOT delete the volume.
	DetachVolume(ctx context.Context, volumeUUID, hostUUID string) error

	// CreateSnapshot freezes the volume's current state under the given
	// name (or a driver-generated one if SnapshotSpec.Name is empty) and
	// returns its descriptor. Drivers that don't support snapshots return
	// ErrUnsupported — file-backed drivers (qcow2 internal snapshots) +
	// cluster drivers (Longhorn / Ceph RBD) implement it.
	CreateSnapshot(ctx context.Context, spec SnapshotSpec) (Snapshot, error)

	// ListSnapshots returns every snapshot known for volumeUUID, in driver-
	// stable order (typically oldest-first by Parent chain). Empty list +
	// nil error means "no snapshots", not "unsupported" — use ErrUnsupported
	// for the latter.
	ListSnapshots(ctx context.Context, volumeUUID string) ([]Snapshot, error)

	// DeleteSnapshot drops a snapshot's on-disk delta. The driver may
	// physically merge it into its parent / child, which can take a while
	// for large deltas. Idempotent : deleting a missing snapshot is a no-op.
	DeleteSnapshot(ctx context.Context, volumeUUID, snapshotName string) error

	// RevertSnapshot rolls the volume back to the given snapshot's state.
	// The volume must be detached first (the driver may enforce this with
	// ErrInUse). Effectively the inverse of CreateSnapshot.
	RevertSnapshot(ctx context.Context, volumeUUID, snapshotName string) error

	// CreateBackup ships a snapshot's contents to the backupstore at
	// spec.Target and returns the resulting backup descriptor. The driver
	// runs the transfer asynchronously when the backupstore is remote ;
	// callers poll ListBackups (the State field) for completion. The
	// snapshot referenced by spec.SnapshotName must already exist.
	CreateBackup(ctx context.Context, spec BackupSpec) (Backup, error)

	// ListBackups enumerates the backups stored at target for volumeUUID
	// (empty volumeUUID = every volume at the target). The target string
	// is the same schema as BackupSpec.Target.
	ListBackups(ctx context.Context, target, volumeUUID string) ([]Backup, error)

	// DeleteBackup removes one backup from the store. Idempotent. The full
	// backup URL (as returned in Backup.URL) is the addressing key — backups
	// can move between volumes during restore, so we don't key by (volume,
	// name).
	DeleteBackup(ctx context.Context, backupURL string) error

	// RestoreBackup creates a new volume (spec) populated from backupURL.
	// The new volume's UUID is spec.UUID ; the driver materialises the
	// backing storage at the requested size (which must be ≥ the original
	// volume size — restore can grow but not shrink). Idempotent : a second
	// restore with the same (spec.UUID, backupURL) is a no-op once the new
	// volume already exists.
	RestoreBackup(ctx context.Context, backupURL string, spec VolumeSpec) error
}
