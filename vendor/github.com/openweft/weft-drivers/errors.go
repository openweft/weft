package drivers

import "errors"

// Sentinel errors driver implementations + callers can compare
// against via errors.Is. Keep this list short — every entry is
// part of the public contract.

var (
	// ErrNotApplicable is returned by a driver when the request
	// is well-formed but doesn't apply to this implementation
	// (e.g. RotateMeshPeer on a non-mesh NetworkDriver).
	ErrNotApplicable = errors.New("driver: operation not applicable to this driver type")

	// ErrUnsupported is returned when the driver knows what's
	// being asked but doesn't (yet) implement it. Different from
	// ErrNotApplicable: the latter means "by design", this one
	// means "patch welcome".
	ErrUnsupported = errors.New("driver: unsupported")

	// ErrNotFound is returned by lookup-shaped methods (LocalPath,
	// resolution helpers) when the queried entity doesn't exist
	// in the driver's view of the world. Lifecycle methods
	// (DeleteVM, DestroyNetwork, …) treat "not found" as success
	// per their idempotence contract — they do NOT return this.
	ErrNotFound = errors.New("driver: not found")

	// ErrInUse is returned by destructive snapshot/backup operations
	// (RevertSnapshot, DeleteSnapshot of the head's parent, …) when
	// the volume is currently attached or otherwise in active use.
	// The caller is expected to detach + retry.
	ErrInUse = errors.New("driver: volume in use")
)
