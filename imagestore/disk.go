//go:build darwin

package imagestore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	disk_qcow2 "github.com/go-diskimages/qcow2"
	disk_tart_oci "github.com/go-diskimages/tart-oci"
	"golang.org/x/sys/unix"
)

// rawCacheName is the conventional filename for the materialised
// raw disk inside a cacheEntryDir. It is created lazily on the
// first CopyImageToDisk that requires a format transcode (qcow2 →
// raw, or OCI LZFSE → raw) so every subsequent clone of the same
// image becomes a single clonefile(2) syscall regardless of source
// format.
const rawCacheName = "raw.img"

// rawMaterialiseLocks serialises concurrent CopyImageToDisk calls
// per imageRef, so two simultaneous CloneVM invocations don't both
// pay the transcode cost. Lazily populated; entries are never
// removed (memory cost is one *sync.Mutex per distinct ref, which
// is unbounded in principle but bounded by the cache itself in
// practice).
var rawMaterialiseLocks sync.Map

func lockFor(ref string) *sync.Mutex {
	v, _ := rawMaterialiseLocks.LoadOrStore(ref, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// CopyImageToDisk materialises a raw disk image at dst from the
// cached imageRef.
//
// On APFS this is a single clonefile(2) for the hot path (cache
// already in raw form) and a one-time transcode-then-clone for
// qcow2/OCI sources. The first call for a given imageRef pays the
// conversion cost; subsequent calls are O(metadata). clonefile
// shares blocks between cache and dst until the guest writes —
// patching the cache file with CachedImagePath after a clone has
// been taken does NOT mutate the clone (APFS COW guarantee), so
// existing VMs keep their content.
//
// Filesystem fallbacks: if the host is not APFS, clonefile(2)
// returns ENOTSUP and we fall through to a streaming byte copy.
// The semantics are otherwise identical.
func (s *store) CopyImageToDisk(imageRef, dst string, w io.Writer) error {
	cacheEntry := s.cacheEntryDir(imageRef)

	// Per-image lock keeps concurrent CloneVM(image=ref) calls from
	// each transcoding the same source. The inner sync.Once-style
	// "stat → materialise → rename" pattern is itself idempotent,
	// but the lock keeps wasted work to zero.
	mu := lockFor(imageRef)
	mu.Lock()
	defer mu.Unlock()

	if isHTTPRef(imageRef) {
		srcPath, isQcow2, err := locateHTTPSource(cacheEntry)
		if err != nil {
			return err
		}
		if !isQcow2 {
			// HTTP raw: the cached file is already a raw disk image,
			// so we can clonefile straight from it — no separate
			// raw.img cache entry needed.
			_, _ = fmt.Fprintf(w, "cloning raw image %s…\n", filepath.Base(srcPath))
			return cloneOrCopyFile(srcPath, dst)
		}
		// HTTP qcow2: ensure a raw cache exists, then clone from it.
		rawPath, err := ensureRawCache(cacheEntry, func(tmp string) error {
			_, _ = fmt.Fprintf(w, "converting qcow2 → raw cache (one-time)…\n")
			return disk_qcow2.ConvertToRaw(srcPath, tmp, w)
		})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "cloning %s…\n", rawCacheName)
		return cloneOrCopyFile(rawPath, dst)
	}

	// OCI tart: same pattern. ExtractDisk decompresses LZFSE blobs
	// into raw — expensive enough that caching the output as raw.img
	// is the whole point of this code path.
	rawPath, err := ensureRawCache(cacheEntry, func(tmp string) error {
		_, _ = fmt.Fprintf(w, "extracting OCI image → raw cache (one-time)…\n")
		return disk_tart_oci.ExtractDisk(cacheEntry, tmp, w)
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "cloning %s…\n", rawCacheName)
	return cloneOrCopyFile(rawPath, dst)
}

// ensureRawCache returns the path to <cacheEntry>/raw.img, creating
// it via `materialise(tmpPath)` if missing. The materialiser writes
// to a .tmp suffix; ensureRawCache renames atomically on success so
// a partial transcode never poisons the cache.
func ensureRawCache(cacheEntry string, materialise func(tmp string) error) (string, error) {
	rawPath := filepath.Join(cacheEntry, rawCacheName)
	if _, err := os.Stat(rawPath); err == nil {
		return rawPath, nil
	}
	tmp := rawPath + ".tmp"
	// A previous interrupted run might have left a .tmp behind —
	// clean it up so the new materialise call starts fresh.
	_ = os.Remove(tmp)
	if err := materialise(tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("materialise raw cache: %w", err)
	}
	if err := os.Rename(tmp, rawPath); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("commit raw cache: %w", err)
	}
	return rawPath, nil
}

// locateHTTPSource picks the cached file for an HTTP ref and
// reports whether it is qcow2. Skips directories (the OCI layout
// case never hits this branch) and the raw cache entry itself.
func locateHTTPSource(cacheEntry string) (string, bool, error) {
	entries, err := os.ReadDir(cacheEntry)
	if err != nil {
		return "", false, fmt.Errorf("read cache dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == rawCacheName || strings.HasSuffix(name, ".tmp") {
			continue
		}
		p := filepath.Join(cacheEntry, name)
		return p, disk_qcow2.IsQCOW2File(p), nil
	}
	return "", false, fmt.Errorf("no image file found in cache dir %s", cacheEntry)
}

// cloneOrCopyFile creates dst as a copy-on-write clone of src via
// macOS clonefile(2) (APFS) and falls back to a streaming byte
// copy when the host filesystem doesn't support clonefile (ENOTSUP)
// or src/dst sit on different volumes (EXDEV).
//
// clonefile fails outright if dst exists; we Remove() it first so
// callers don't have to.
func cloneOrCopyFile(src, dst string) error {
	_ = os.Remove(dst)
	err := unix.Clonefile(src, dst, 0)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EXDEV) {
		// Any other errno is a real failure — surface it. Common
		// suspects: ENOENT (src vanished), EACCES (parent dir
		// unwritable), ENOSPC (out of space).
		return fmt.Errorf("clonefile %s → %s: %w", src, dst, err)
	}
	return copyFileRaw(src, dst)
}

// CachedImagePath returns the absolute path to the raw (non-qcow2) disk image
// file in the local cache for imageRef.
//
// Only HTTP-sourced raw images are patchable in-place. QCOW2 images must be
// converted first (use CopyImageToDisk). OCI images are stored as OCI layout
// blobs and cannot be patched via CachedImagePath.
//
// Returns an error if the image is not in the cache, is QCOW2, or is an OCI ref.
func (s *store) CachedImagePath(imageRef string) (string, error) {
	if !isHTTPRef(imageRef) {
		return "", fmt.Errorf("CachedImagePath: OCI images cannot be patched in place (ref %s)", imageRef)
	}
	cacheEntry := s.cacheEntryDir(imageRef)
	srcPath, isQcow2, err := locateHTTPSource(cacheEntry)
	if err != nil {
		return "", fmt.Errorf("CachedImagePath: %w (ref %s)", err, imageRef)
	}
	if isQcow2 {
		return "", fmt.Errorf("CachedImagePath: QCOW2 images must be converted before patching (ref %s)", imageRef)
	}
	return srcPath, nil
}

// copyFileRaw copies src to dst as a plain byte stream. Used as
// the non-APFS fallback path for cloneOrCopyFile.
func copyFileRaw(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open dest: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close dest: %w", err)
	}
	return nil
}
