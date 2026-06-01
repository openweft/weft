// local.go : Backend implementation that copies snapshot blobs
// into a directory tree under a single root. The root is meant
// to be a mounted NFS / SMB / SSHFS share (operator's
// responsibility) so the data sits off-host even though the API
// surface is pure filesystem.
//
// Layout: keys are joined onto the root as-is, with the
// "<p>/<v>/<s>.qcow2" convention from keys.go producing a tidy
// three-level hierarchy. Subdirectories are created lazily on
// Upload.
//
// Atomicity: Upload writes via tmp-file + rename so concurrent
// readers never see a half-uploaded blob. Download uses a similar
// dance into a `dstPath + ".part"` then renames into place. Delete
// is idempotent on ENOENT.
//
// Path-traversal safety : key is filepath.Clean'd and joined with
// the root, then re-checked against the root prefix. A key with
// "../" segments that escape the root is rejected before any I/O
// (defence-in-depth against ill-formed operator input).

package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LocalBackend is the dev / air-gapped Backend implementation.
// Root is the directory keys are joined under. Root must exist
// before Upload is called — we don't auto-create the top-level
// root because that would mask a misconfigured mount point.
type LocalBackend struct {
	// Root is the absolute path under which keys land. A trailing
	// slash is tolerated but not required.
	Root string
}

// NewLocalBackend returns a LocalBackend rooted at root. Returns
// an error if root is empty ; we don't Stat the root here because
// the operator might be configuring weft before the NFS mount
// lands (boot-order dependency). Operations that need the root
// existing will surface their own ENOENT.
func NewLocalBackend(root string) (*LocalBackend, error) {
	if root == "" {
		return nil, errors.New("backup/local: root is required")
	}
	return &LocalBackend{Root: root}, nil
}

var _ Backend = (*LocalBackend)(nil)

// resolveKey joins key onto Root and guards against path traversal.
// Returns the absolute file path or an error if key escapes Root.
func (b *LocalBackend) resolveKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("backup/local: empty key")
	}
	clean := filepath.Clean(key)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("backup/local: key %q escapes root", key)
	}
	full := filepath.Join(b.Root, clean)
	// Re-check the prefix : filepath.Join with sneaky inputs
	// shouldn't escape but the Cleaner could collapse a leading
	// "./" away in a way that doesn't trip the first guard.
	rootClean := filepath.Clean(b.Root) + string(filepath.Separator)
	if !strings.HasPrefix(full+string(filepath.Separator), rootClean) {
		return "", fmt.Errorf("backup/local: key %q escapes root", key)
	}
	return full, nil
}

// Upload copies srcPath → Root/key atomically. The destination's
// parent directory is mkdir-p'd ; the write goes to a sibling
// tmp-file then rename(2)s into place. Caller-supplied ctx is
// honoured between read chunks.
func (b *LocalBackend) Upload(ctx context.Context, srcPath, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if srcPath == "" {
		return errors.New("backup/local: empty srcPath")
	}
	dst, err := b.resolveKey(key)
	if err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("backup/local: open src: %w", err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("backup/local: mkdir parent: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*.tmp")
	if err != nil {
		return fmt.Errorf("backup/local: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail mid-write.
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if err := copyWithCtx(ctx, tmp, src); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("backup/local: copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup/local: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("backup/local: rename: %w", err)
	}
	return nil
}

// Download copies Root/key → dstPath atomically (writes
// dstPath+".part" then rename). Returns ErrNotFound if key is
// absent on the backend.
func (b *LocalBackend) Download(ctx context.Context, key, dstPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dstPath == "" {
		return errors.New("backup/local: empty dstPath")
	}
	src, err := b.resolveKey(key)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup/local: %q: %w", key, ErrNotFound)
		}
		return fmt.Errorf("backup/local: open key: %w", err)
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("backup/local: mkdir dst parent: %w", err)
	}
	part := dstPath + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("backup/local: open part: %w", err)
	}
	if err := copyWithCtx(ctx, out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(part)
		return fmt.Errorf("backup/local: copy: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("backup/local: close part: %w", err)
	}
	if err := os.Rename(part, dstPath); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("backup/local: rename: %w", err)
	}
	return nil
}

// List walks Root and returns every entry whose backend-relative
// path starts with prefix. Directories are skipped ; only regular
// files become Entry rows. An absent Root yields an empty slice
// rather than an error (consistent with "no keys yet"); any
// other Stat / Walk error bubbles up.
func (b *LocalBackend) List(ctx context.Context, prefix string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []Entry
	root := filepath.Clean(b.Root)
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("backup/local: stat root: %w", err)
	}
	err := filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		// Normalise to forward slashes so keys are platform-stable
		// (the convention from keys.go uses '/' separators).
		rel = filepath.ToSlash(rel)
		if prefix != "" && !strings.HasPrefix(rel, prefix) {
			return nil
		}
		out = append(out, Entry{Key: rel, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("backup/local: walk: %w", err)
	}
	return out, nil
}

// Delete removes Root/key. Missing keys return nil (idempotent).
func (b *LocalBackend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := b.resolveKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup/local: remove: %w", err)
	}
	return nil
}

// copyWithCtx is io.Copy with periodic ctx.Err checks. Chunk size
// is large enough that the per-chunk syscall overhead is negligible
// on multi-GiB qcow2 blobs but small enough that an operator
// Ctrl-C cancels within ~milliseconds.
func copyWithCtx(ctx context.Context, dst io.Writer, src io.Reader) error {
	const chunk = 1 << 20 // 1 MiB
	buf := make([]byte, chunk)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
