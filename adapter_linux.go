//go:build linux

package weft

// adapter_linux.go is the Linux datapath for the cross-platform Adapter:
// QEMU/KVM driver bundle, a plain-copy share stager (no APFS clonefile), and a
// reflink-backed classic-VM image store (copy-on-write clones of pre-staged raw
// images; OCI/qcow2 pull stays darwin-only).

import (
	"github.com/openweft/weft/imagestore"
)

// defaultDriverPlugin is the local hypervisor plugin launched on non-darwin
// hosts: the pure-Go QEMU driver (KVM when nested virt is present, else TCG —
// selected inside the plugin).
const defaultDriverPlugin = "weft-driver-qemu"

// newImageStore returns the reflink-backed classic-VM image store: it clones
// pre-staged raw golden images into per-VM disks via copy-on-write reflink
// (FICLONE on ZFS ≥ 2.2.2 / btrfs / XFS), falling back to a byte copy. OCI/HTTP
// pull and qcow2 conversion remain darwin-only — stage raw images into the
// cache dir, or pass an absolute image path (e.g. a golden raw on a ZFS
// dataset). See imagestore.NewReflink.
func newImageStore(dir string) imagestore.ImageStore { return imagestore.NewReflink(dir) }

// cloneOrCopyTree stages a share by recursive copy (reflink for whole trees is
// not wired yet; clones happen per file via the image store).
func cloneOrCopyTree(src, dst string) error { return copyTree(src, dst) }
