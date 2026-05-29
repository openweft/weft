// Package cowclone creates copy-on-write clones of files, falling back to a
// plain byte copy when the host filesystem can't share blocks.
//
// On a copy-on-write filesystem the clone is O(metadata) and shares blocks
// with the source until one side is written:
//
//   - darwin: APFS clonefile(2)
//   - linux:  FICLONE ioctl (reflink) — ZFS with OpenZFS ≥ 2.2 block
//     cloning, btrfs, XFS
//
// Everywhere else — or across filesystems, or on a non-CoW filesystem — it
// degrades to a full byte copy with identical observable semantics. Callers
// get an independent dst either way; the only difference is cost and whether
// space is shared with src.
package cowclone

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// errCoWUnsupported is the sentinel the per-OS cloneCoW implementations return
// when the platform or filesystem can't do a block-sharing clone (or src and
// dst live on different filesystems). It tells Clone to fall back to a byte
// copy rather than surface the error. Any other error is a genuine failure.
var errCoWUnsupported = errors.New("cowclone: copy-on-write clone unsupported")

// cloneCoWFn is the copy-on-write clone primitive, indirected through a var so
// tests can force the byte-copy fallback (or a hard error) deterministically on
// any filesystem — clonefile/reflink succeeds on the test host's tmp dir, so
// the fallback path is otherwise unreachable in-process. Production code never
// reassigns it.
var cloneCoWFn = cloneCoW

// Clone makes dst a copy-on-write clone of src, replacing dst if it already
// exists.
//
// It attempts a real CoW clone first (APFS clonefile / Linux FICLONE reflink).
// On a filesystem that can't share blocks, or when src and dst sit on
// different filesystems, it transparently falls back to a streaming byte copy.
// The result is observably the same: dst is a full, independent copy of src.
func Clone(src, dst string) error {
	// A CoW clone fails if dst already exists, so remove it first — callers
	// (and the byte-copy fallback's O_TRUNC) shouldn't have to care.
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cowclone: remove dst %s: %w", dst, err)
	}

	err := cloneCoWFn(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errCoWUnsupported) {
		// A real failure — ENOENT (src vanished), EACCES (dir unwritable),
		// ENOSPC (out of space), … — surface it.
		return fmt.Errorf("cowclone: clone %s → %s: %w", src, dst, err)
	}

	// The filesystem can't share blocks (or src/dst are cross-device): copy.
	if err := copyFile(src, dst); err != nil {
		return fmt.Errorf("cowclone: copy %s → %s: %w", src, dst, err)
	}
	return nil
}

// copyFile streams src to a freshly created dst. It is the fallback when a CoW
// clone isn't possible, and is exercised directly by the per-OS implementations
// only via Clone.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy: %w", err)
	}
	// Close explicitly (not deferred) so a flush/commit error isn't swallowed.
	if err := out.Close(); err != nil {
		return fmt.Errorf("close dst: %w", err)
	}
	return nil
}
