package tartoci

import (
	"fmt"
	"io"
	"os"
)

// Format is the Tart OCI disk-image format adapter. It lets a locally-cached
// Tart OCI layout compose with the github.com/go-diskimages format registry: a
// Format value structurally satisfies the diskimage_format.Format interface
// (Name/Create/Detect/ToRaw/Resize) without importing that module.
//
// For Create, Detect and ToRaw the "path" argument is a directory in OCI
// image-layout form. To fetch an image over the network instead, use PullDisk
// or Pull.
type Format struct{}

// Name returns "tart-oci".
func (Format) Name() string { return "tart-oci" }

// Create writes a new Tart OCI layout at path whose single disk layer
// decompresses to sizeBytes of zeros. The whole disk is built in memory, so
// Create is intended for tests and small bootstrap images.
func (Format) Create(path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("tart-oci: Create: size must be positive, got %d", sizeBytes)
	}
	return writeLayout(path, make([]byte, sizeBytes))
}

// Detect reports whether path is a directory holding a Tart OCI layout with at
// least one reachable disk.v2 layer.
func (Format) Detect(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	m, err := readManifest(path)
	if err != nil {
		return false, nil //nolint:nilerr // an unreadable layout is simply "not this format"
	}
	return len(m.DiskLayers()) > 0, nil
}

// ToRaw extracts the raw disk image from the Tart OCI layout at src to dst,
// reporting progress to w.
func (Format) ToRaw(src, dst string, w io.Writer) error {
	return ExtractDisk(src, dst, w)
}

// Resize is not supported: a Tart disk layer is compressed and content-addressed,
// so resizing would invalidate its digests. Create a new image instead.
func (Format) Resize(path string, newSizeBytes int64) error {
	return fmt.Errorf("tart-oci: Resize is not supported; create a new image instead")
}
