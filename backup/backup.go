// Package backup is the off-host backup target abstraction for the
// VolumeSnapshot feature. It exposes a Backend interface that any
// blob-store (S3-compatible buckets, NFS-mounted filesystems, …)
// can satisfy, plus two ready-made implementations:
//
//   - LocalBackend (local.go) — copies blobs to a directory tree
//     under a single root. Used for dev + air-gapped deployments
//     where the operator mounts an NFS share at a stable path.
//
//   - S3Backend (s3.go) — talks to any S3-compatible HTTP API
//     (AWS S3, MinIO, Backblaze B2, Wasabi, Cloudflare R2 …) via
//     the stdlib net/http + a small AWS Signature Version 4 signer.
//     We don't pull in aws-sdk-go-v2 because (1) it's not in go.sum
//     already, (2) the surface we need is tiny (PUT/GET/LIST/DELETE),
//     and (3) the SDK is ~50MB of vendored code for what amounts to
//     four HTTP verbs.
//
// Why a package interface rather than wiring straight into
// volumesnapshots.go : the upload step is fire-and-forget on the
// happy path (the local reflink clone is already durable enough
// for restore-on-same-host) and gains its value across host loss.
// Surfacing the seam early lets us add backends without touching
// the snapshot code path, and makes "no backend configured" a
// degenerate case of "Backend is nil" rather than a branch.
//
// Concurrency model : a Backend's methods must be safe to call
// concurrently — both the cmd/weft client and (later) the agent
// upload pipeline will spawn goroutines that hit it in parallel.
// The two reference implementations satisfy this trivially (Local
// is pure filesystem ops, S3 uses http.DefaultClient transports
// which are goroutine-safe).
//
// Key shape : the snapshot CLI uses
// `<project_uuid>/<volume_uuid>/<snapshot_uuid>.qcow2`. The
// Backend treats keys as opaque strings — no enforcement, no
// rewriting. Parsing is in keys.go for consumers that want to
// validate or extract identifiers.
package backup

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Download / List / Delete when the
// caller-supplied key does not exist on the backend. Callers can
// errors.Is(err, ErrNotFound) to differentiate "missing blob" from
// "I/O failure". Each backend wraps a more descriptive message
// around this sentinel so an operator-facing error still names
// the key and the backend.
var ErrNotFound = errors.New("backup: key not found")

// Entry is one row returned by Backend.List. The backend converts
// its native shape (S3 ListObjectsV2 contents, filesystem Stat,
// etc.) into this minimum surface ; downstream callers (weft
// restore CLI, ops scripts) read Key + Size and ignore the rest.
type Entry struct {
	// Key is the backend-relative key, identical to what would be
	// passed back to Download / Delete.
	Key string
	// Size is the blob's size in bytes. -1 means "unknown" (a few
	// S3-compatibles omit it for in-progress uploads).
	Size int64
}

// Backend is the off-host blob store contract. All four ops are
// idempotent against retries the way an ops-level workflow expects :
//
//   - Upload(key) overwrites if key already exists. Atomicity is
//     best-effort ; an interrupted upload may leave a partial blob
//     on the backend. Restore-time verification (size + format
//     sniff) is the operator's safety net.
//
//   - Download(key) writes the destination atomically (tmp-file
//     then rename) on backends that support it. Returns ErrNotFound
//     if the key is absent.
//
//   - List(prefix) returns the union of keys that share the prefix.
//     Pagination is hidden ; large lists are buffered in memory.
//     Returns an empty slice (not nil) when the prefix has no
//     matches, never ErrNotFound.
//
//   - Delete(key) is idempotent : deleting a missing key returns
//     nil. Matches LocalBackend's os.Remove ENOENT handling.
//
// The ctx parameter is honoured by every implementation : long
// uploads / downloads check ctx.Err on chunk boundaries so an
// operator who Ctrl-Cs the weft CLI doesn't leave a dangling
// goroutine pumping bytes.
type Backend interface {
	Upload(ctx context.Context, srcPath, key string) error
	Download(ctx context.Context, key, dstPath string) error
	List(ctx context.Context, prefix string) ([]Entry, error)
	Delete(ctx context.Context, key string) error
}
