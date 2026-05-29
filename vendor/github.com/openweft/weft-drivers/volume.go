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
}
