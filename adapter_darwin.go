//go:build darwin

package weft

// adapter_darwin.go holds what stays macOS-specific after the driver datapath
// moved into external plugins: the real OCI/qcow2 image store and APFS
// clonefile share staging. The hypervisor itself is now the weft-driver-vz
// plugin (see localDriverBundle in adapter.go), so there's no cgo here.

import (
	"errors"

	"github.com/openweft/weft/imagestore"
	"golang.org/x/sys/unix"
)

// defaultDriverPlugin is the local hypervisor plugin launched on macOS unless
// --hypervisor=qemu overrides it.
const defaultDriverPlugin = "weft-driver-vz"

// newImageStore returns the real OCI/qcow2 image cache (clonefile-backed).
func newImageStore(dir string) imagestore.ImageStore { return imagestore.New(dir) }

// cloneOrCopyTree stages a share via APFS clonefile (O(metadata) CoW),
// falling back to a recursive copy on non-APFS volumes.
func cloneOrCopyTree(src, dst string) error {
	if err := unix.Clonefile(src, dst, 0); err == nil {
		return nil
	} else if !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EXDEV) {
		return err
	}
	return copyTree(src, dst)
}
