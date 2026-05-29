//go:build !darwin

package tartoci

import (
	"fmt"
	"io"
)

// Format is the Tart OCI disk image format.
// On non-darwin platforms, all operations return an error.
type Format struct{}

// Name returns "tart-oci".
func (Format) Name() string { return "tart-oci" }

// Create is not supported on non-darwin platforms.
func (Format) Create(path string, sizeBytes int64) error {
	return fmt.Errorf("tart-oci: only supported on darwin")
}

// Detect is not supported on non-darwin platforms.
func (Format) Detect(path string) (bool, error) {
	return false, fmt.Errorf("tart-oci: only supported on darwin")
}

// ToRaw is not supported on non-darwin platforms.
func (Format) ToRaw(src, dst string, _ io.Writer) error {
	return fmt.Errorf("tart-oci: only supported on darwin")
}

// Resize is not supported on non-darwin platforms.
func (Format) Resize(path string, newSizeBytes int64) error {
	return fmt.Errorf("tart-oci: only supported on darwin")
}
