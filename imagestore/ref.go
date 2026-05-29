package imagestore

import (
	"context"
	"io"
	"strings"
)

// ImageStore is the local image cache used by the (classic-VM) datapath. The
// interface is cross-platform; the concrete implementation (New) is
// darwin-only (oras/OCI pull + APFS clonefile / qcow2 conversion in store.go
// + disk.go). Non-darwin builds wire a stub — the microVM/QEMU path doesn't
// use this classic-VM image cache.
type ImageStore interface {
	// SetChecksums stores validation URLs so PullImage can verify HTTP downloads.
	SetChecksums(checksums map[string]string)
	// Dir returns the path to the cache directory.
	Dir() string
	// SetDir updates the cache directory.
	SetDir(dir string)
	// PullImage downloads ref into the local cache, writing progress to w.
	PullImage(ctx context.Context, ref string, w io.Writer) error
	// ImageInCache reports whether ref is already present in the local cache.
	ImageInCache(ref string) bool
	// ListOCI returns cached OCI images as a list of property maps.
	ListOCI() ([]map[string]interface{}, error)
	// DeleteOCI removes a cached image entry by name.
	DeleteOCI(name string) error
	// CachedImagePath returns the absolute path to the raw disk image in cache.
	CachedImagePath(imageRef string) (string, error)
	// CopyImageToDisk copies the cached image for imageRef to dst as a raw disk image.
	CopyImageToDisk(imageRef, dst string, w io.Writer) error
}

// SanitizeRef converts an OCI image ref to a filesystem-safe directory name.
// It replaces "@" with "___", ":" with "__", and "/" with "_".
// The replacements are length-increasing to allow unambiguous reversal.
func SanitizeRef(ref string) string {
	r := strings.ReplaceAll(ref, "@", "___")
	r = strings.ReplaceAll(r, ":", "__")
	r = strings.ReplaceAll(r, "/", "_")
	return r
}

// UnsanitizeRef reverses SanitizeRef (longest patterns first).
func UnsanitizeRef(s string) string {
	r := strings.ReplaceAll(s, "___", "@")
	r = strings.ReplaceAll(r, "__", ":")
	r = strings.ReplaceAll(r, "_", "/")
	return r
}
