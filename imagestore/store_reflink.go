package imagestore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/openweft/weft/cowclone"
)

// reflinkRawName mirrors the darwin cache convention (rawCacheName in disk.go):
// a staged raw golden image for <ref> lives at <dir>/<SanitizeRef(ref)>/raw.img.
const reflinkRawName = "raw.img"

// reflinkStore is a minimal, cross-platform ImageStore for hosts without the
// darwin OCI/qcow2 pull+transcode machinery (i.e. Linux). It does not pull or
// convert images; it treats the cache dir as a set of pre-staged raw golden
// images and materialises per-VM disks from them with copy-on-write reflink —
// FICLONE on ZFS (OpenZFS ≥ 2.2.2 block cloning), btrfs, or XFS — via
// cowclone.Clone, falling back to a byte copy on a non-CoW filesystem.
//
// This is the Linux counterpart of the darwin clonefile path: the per-VM
// disk.img shares blocks with the golden image until the guest writes, so
// spawning N instances from one image costs O(metadata) and ~zero extra space.
// For the reflink (not the copy) path to engage, the cache dir and the VM
// state dir must live on the same pool/dataset — across filesystems the kernel
// returns EXDEV and cowclone falls back to a full copy.
//
// An imageRef is resolved to a source path one of two ways:
//
//   - an absolute path → used as-is (point straight at a golden image, e.g.
//     /tank/images/debian-13.raw on a ZFS dataset);
//   - otherwise → <dir>/<SanitizeRef(ref)>/raw.img, the same layout the darwin
//     store materialises, so a cache populated out-of-band is reused verbatim.
//
// Pulling, qcow2 conversion and OCI listing remain unsupported here; stage raw
// images into the cache dir (or pass an absolute path) instead.
type reflinkStore struct{ dir string }

// NewReflink returns a reflink-backed ImageStore rooted at the given cache dir.
func NewReflink(dir string) ImageStore { return &reflinkStore{dir: dir} }

var _ ImageStore = (*reflinkStore)(nil)

// errReflinkPullUnsupported reports the pull/convert/list machinery as
// unavailable on this host — only staged raw images can be cloned.
var errReflinkPullUnsupported = fmt.Errorf(
	"imagestore: OCI/HTTP pull, qcow2 conversion and listing are darwin-only; " +
		"stage a raw image in the cache dir or pass an absolute image path")

func (s *reflinkStore) SetChecksums(map[string]string) {}
func (s *reflinkStore) Dir() string                    { return s.dir }
func (s *reflinkStore) SetDir(dir string)              { s.dir = dir }

// resolve maps an image ref to its source raw-image path (see the type doc).
func (s *reflinkStore) resolve(ref string) string {
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(s.dir, SanitizeRef(ref), reflinkRawName)
}

// ImageInCache reports whether ref resolves to a staged regular file.
func (s *reflinkStore) ImageInCache(ref string) bool {
	fi, err := os.Stat(s.resolve(ref))
	return err == nil && fi.Mode().IsRegular()
}

// CachedImagePath returns the resolved source path, erroring if it isn't a
// staged regular file.
func (s *reflinkStore) CachedImagePath(ref string) (string, error) {
	p := s.resolve(ref)
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("imagestore: image %q not staged at %s: %w", ref, p, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("imagestore: staged image %s is not a regular file", p)
	}
	return p, nil
}

// CopyImageToDisk clones the staged raw image for ref into dst via copy-on-write
// reflink, falling back to a byte copy when blocks can't be shared.
func (s *reflinkStore) CopyImageToDisk(ref, dst string, w io.Writer) error {
	src, err := s.CachedImagePath(ref)
	if err != nil {
		return err
	}
	if w != nil {
		_, _ = fmt.Fprintf(w, "cloning %s → %s (reflink, copy fallback)…\n",
			filepath.Base(src), filepath.Base(dst))
	}
	if err := cowclone.Clone(src, dst); err != nil {
		return fmt.Errorf("imagestore: clone %s → %s: %w", src, dst, err)
	}
	return nil
}

func (s *reflinkStore) PullImage(context.Context, string, io.Writer) error {
	return errReflinkPullUnsupported
}
func (s *reflinkStore) ListOCI() ([]map[string]interface{}, error) {
	return nil, errReflinkPullUnsupported
}
func (s *reflinkStore) DeleteOCI(string) error { return errReflinkPullUnsupported }
