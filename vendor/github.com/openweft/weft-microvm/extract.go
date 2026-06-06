// Extract OCI image layers (tar streams, usually gzipped) into a
// destination directory, applying OCI image-spec whiteout rules.
//
// OCI whiteouts (image-spec, layer.md §"Whiteouts"):
//
//   - A regular file named `.wh.<name>` next to a directory entry
//     means: delete `<name>` in the destination before applying any
//     more entries from this layer. The marker itself is not
//     extracted.
//   - A regular file named `.wh..wh..opq` inside a directory means:
//     remove everything currently in that directory before applying
//     this layer's entries. The marker itself is not extracted.
//
// We process layers in order (base → topmost). Each layer is a
// tar(.gz) stream; we walk its entries, apply whiteouts first, then
// materialise the rest.
//
// Symlinks, hardlinks, char/block devices, fifos are handled with
// the standard tar header conventions. We skip device nodes (no
// CAP_MKNOD needed; the container's userspace doesn't typically
// rely on them living in the extracted rootfs — /dev gets
// bind-mounted at runtime by weft-microvm-init).

package microvm

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// extractLayer reads `r` (gzip-compressed tar) and writes entries
// under `dest`. Returns once the stream ends. The caller is
// responsible for repeating this across layers.
func extractLayer(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip open: %w", err)
	}
	defer gz.Close()
	return extractTar(gz, dest)
}

// extractTar reads an uncompressed tar from `r` and writes entries
// under `dest`. Some registries serve layers as `application/vnd.oci.
// image.layer.v1.tar` (no gzip); extract.go's caller can pick this
// directly when the descriptor says so.
func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if err := applyEntry(tr, hdr, dest); err != nil {
			return fmt.Errorf("entry %q: %w", hdr.Name, err)
		}
	}
}

// applyEntry handles one tar header.
func applyEntry(tr io.Reader, hdr *tar.Header, dest string) error {
	cleanName, err := safeJoin(dest, hdr.Name)
	if err != nil {
		return err
	}

	// Whiteout handling: opaque-dir, then per-entry.
	base := filepath.Base(hdr.Name)
	if base == ".wh..wh..opq" {
		// Remove every child of the parent dir; do not extract the marker.
		parent := filepath.Dir(cleanName)
		entries, err := os.ReadDir(parent)
		if err == nil {
			for _, e := range entries {
				_ = os.RemoveAll(filepath.Join(parent, e.Name()))
			}
		}
		return nil
	}
	if strings.HasPrefix(base, ".wh.") {
		// Delete <target> in the parent dir.
		target := filepath.Join(filepath.Dir(cleanName), strings.TrimPrefix(base, ".wh."))
		_ = os.RemoveAll(target)
		return nil
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(cleanName, os.FileMode(hdr.Mode)&os.ModePerm); err != nil {
			return err
		}
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(cleanName), 0o755); err != nil {
			return err
		}
		// Standard OCI layer-replace path : a later layer can replace a
		// file dropped by an earlier one. If the existing file landed
		// with mode 0444 / 0444 (read-only ; common on /etc/ssl/certs/
		// ca-certificates.crt + many Debian config files), O_TRUNC fails
		// with EACCES before we can truncate it. Remove-and-recreate is
		// what Docker / podman / containerd extractors do — we mirror.
		mode := os.FileMode(hdr.Mode) & os.ModePerm
		f, err := os.OpenFile(cleanName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil && os.IsPermission(err) {
			if rmErr := os.Remove(cleanName); rmErr == nil {
				f, err = os.OpenFile(cleanName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			}
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		// Preserve mtime so tools that rely on it (e.g. make) see
		// stable values. We don't preserve atime.
		_ = os.Chtimes(cleanName, time.Now(), hdr.ModTime)
	case tar.TypeSymlink:
		_ = os.MkdirAll(filepath.Dir(cleanName), 0o755)
		// Recreate the link; tolerate existing entry by removing first.
		_ = os.Remove(cleanName)
		if err := os.Symlink(hdr.Linkname, cleanName); err != nil {
			return err
		}
	case tar.TypeLink:
		_ = os.MkdirAll(filepath.Dir(cleanName), 0o755)
		linkTarget, err := safeJoin(dest, hdr.Linkname)
		if err != nil {
			return err
		}
		_ = os.Remove(cleanName)
		if err := os.Link(linkTarget, cleanName); err != nil {
			return err
		}
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Skip device nodes; weft-microvm-init's /dev is a bind mount and
		// the extracted rootfs entries would be no-ops anyway.
		return nil
	default:
		// xattr / sparse / global-header tar types we don't care about.
		return nil
	}
	return nil
}

// safeJoin defends against tar entries with absolute or escaping
// paths.
//
//   - Leading slashes are stripped (entry treated as relative).
//   - After Clean, anything starting with "../" or equal to ".."
//     is rejected — that's an escape attempt and we don't honour
//     "neutralise into a safe sibling" because hardlink/symlink
//     entries (where Linkname matters) could still be abused.
//
// Returns the resolved absolute path on success.
func safeJoin(dest, name string) (string, error) {
	name = strings.TrimLeft(name, "/")
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("tar entry escapes destination")
	}
	return filepath.Join(dest, clean), nil
}
