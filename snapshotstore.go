package weft

// snapshotstore.go is the on-disk side of the VolumeSnapshot
// feature: a thin, file-per-snapshot store that reflink-clones a
// parent volume image into <dir>/<uuid>.bin via cowclone.Clone.
//
// Why a dedicated type, mirroring imagestore.NewReflink instead of
// reusing it: image-store ops are keyed by an opaque image ref
// (sanitised) and resolve to <ref-dir>/raw.img — a one-image-many-
// clones cache shape. Snapshots are keyed by their own UUID, are
// one-per-file, and have no name-sanitisation: the caller already
// owns the UUID. Same CoW primitive (cowclone.Clone — APFS
// clonefile / Linux FICLONE, byte-copy fallback on non-CoW FSes),
// different addressing.
//
// Disk layout: every snapshot lives at <dir>/<snapshot-uuid>.bin.
// Delete is idempotent: removing an already-absent file is fine
// (so the registry-then-blob ordering in volumesnapshots.go can
// retry the cleanup cheaply).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openweft/weft/cowclone"
)

// snapshotFileExt is the on-disk suffix for a snapshot blob. Keeps
// the layout greppable when an operator pokes around the state dir.
const snapshotFileExt = ".bin"

// SnapshotStore writes / reads / removes the on-disk blob backing
// one VolumeSnapshot. Create reflink-clones the parent volume's
// image at parentVolumePath into snapshotPath ; Delete drops the
// snapshot file (idempotent on ENOENT — see the file header).
type SnapshotStore interface {
	Create(ctx context.Context, parentVolumePath, snapshotPath string) error
	Delete(ctx context.Context, snapshotPath string) error
}

// reflinkSnapshotStore is the cowclone-backed SnapshotStore. dir is
// the on-disk root that holds every <uuid>.bin file ; the adapter
// derives child paths via SnapshotPath.
type reflinkSnapshotStore struct{ dir string }

// NewReflinkSnapshotStore returns a SnapshotStore rooted at dir.
// Mirrors imagestore.NewReflink in shape so the construction sites
// in adapter.go read symmetrically.
func NewReflinkSnapshotStore(dir string) SnapshotStore {
	return &reflinkSnapshotStore{dir: dir}
}

var _ SnapshotStore = (*reflinkSnapshotStore)(nil)

// Dir returns the configured root, exposed for diagnostics + tests.
func (s *reflinkSnapshotStore) Dir() string { return s.dir }

// Create clones parentVolumePath → snapshotPath via cowclone.Clone.
// It ensures the destination's parent dir exists ; the snapshot
// file is overwritten if already present (cowclone.Clone removes
// dst first, so its semantics carry through).
func (s *reflinkSnapshotStore) Create(ctx context.Context, parentVolumePath, snapshotPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if parentVolumePath == "" {
		return errors.New("snapshotstore: empty parent volume path")
	}
	if snapshotPath == "" {
		return errors.New("snapshotstore: empty snapshot path")
	}
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o755); err != nil {
		return fmt.Errorf("snapshotstore: mkdir snapshot dir: %w", err)
	}
	if err := cowclone.Clone(parentVolumePath, snapshotPath); err != nil {
		return fmt.Errorf("snapshotstore: clone snapshot: %w", err)
	}
	return nil
}

// Delete removes snapshotPath. A missing file is not an error: the
// caller (volumesnapshots.go) deletes the registry row before the
// blob, so re-running a half-failed delete must converge.
func (s *reflinkSnapshotStore) Delete(ctx context.Context, snapshotPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshotPath == "" {
		return errors.New("snapshotstore: empty snapshot path")
	}
	if err := os.Remove(snapshotPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("snapshotstore: remove %s: %w", snapshotPath, err)
	}
	return nil
}

// SnapshotPath returns the canonical on-disk file for the given
// snapshot UUID, rooted at the store's dir. Exposed (lower-case ok
// for in-package consumers ; the adapter is the only caller) so
// volumesnapshots.go can resolve the path without re-deriving the
// layout.
func snapshotPathIn(dir, uuid string) string {
	return filepath.Join(dir, uuid+snapshotFileExt)
}
