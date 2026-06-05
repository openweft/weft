package microvm

import (
	"fmt"
	"os"
)

// PreparedBoot is the bundle a `weft RegisterMicroVM` consumer
// (typically the weft daemon's server-side handler) plugs into the
// boot + shares fields after resolving an OCI image.
//
// Mirrors what runMicroVM assembles client-side ; the goal of
// Prepare is to expose that same translation to any in-process
// caller — so the gRPC RegisterMicroVM handler can accept a bare
// `image` field and do the OCI pull + share assembly server-side
// without forcing every client (Go, Python, Terraform) to
// re-implement the dance.
type PreparedBoot struct {
	// Name is the conventional "weft-microvm-<refsafe>". Callers that
	// already have a name on the request override this ; callers that
	// don't (eg. weft-network's lifecycle.Ensure passing the Router
	// uuid) plug it in directly.
	Name string

	// Kernel + Initrd : populated in direct-Linux mode (the default
	// produced by the weft-microvm-init kernel + ncl-init initrd in
	// the local cache). Mutually exclusive with BootISO.
	Kernel string
	Initrd string

	// BootISO : populated only in UKI mode (when locateBootArtefacts
	// finds a boot.iso but no separate kernel).
	BootISO string

	// Cmdline synthesised for the weft-microvm-init guest. Carries
	// the rootfs share tag so the guest knows which virtio-fs share
	// to pivot-root into : "weft.rootfs=virtiofs:<tag> console=hvc0".
	Cmdline string

	// SharePath is the host-side path to the extracted OCI rootfs
	// (post-Pull). ShareTag is the virtio-fs / 9p tag the guest
	// mounts at boot — default "rootfs0".
	SharePath string
	ShareTag  string
}

// Prepare resolves an OCI image into the artefacts RegisterMicroVM
// needs. Pure host-side prep — doesn't dial weft, doesn't register
// anything ; the caller does that with the returned PreparedBoot.
//
// Lifecycle :
//  1. Compute the cache path for the image's rootfs.
//  2. If missing on disk, Pull() it (matches `docker run`'s auto-pull
//     UX). Honors WEFT_NO_AUTO_PULL=1 for strict offline mode.
//  3. Locate the boot artefacts (the shared weft-microvm-init kernel
//     + initrd cached under $XDG_DATA_HOME/weft-microvm/, or the
//     fallback UKI boot.iso).
//  4. Synthesise the cmdline + share tag.
//
// Returns InvalidArgument when image is empty.
func Prepare(image string) (*PreparedBoot, error) {
	if image == "" {
		return nil, fmt.Errorf("prepare: image is required")
	}
	rs := refsafe(image)
	rootfs := rootfsPath(rs)

	if _, err := os.Stat(rootfs); err != nil {
		if os.Getenv("WEFT_NO_AUTO_PULL") == "1" {
			return nil, fmt.Errorf("image %q not pulled and WEFT_NO_AUTO_PULL=1 (missing rootfs at %s)", image, rootfs)
		}
		if err := Pull(image); err != nil {
			return nil, fmt.Errorf("auto-pull %s: %w", image, err)
		}
	}

	iso, err := locateBootArtefacts()
	if err != nil {
		return nil, err
	}

	const tag = "rootfs0"
	return &PreparedBoot{
		Name:      "weft-microvm-" + rs,
		Kernel:    iso.kernel,
		Initrd:    iso.initrd,
		BootISO:   iso.bootISO,
		Cmdline:   fmt.Sprintf("weft.rootfs=virtiofs:%s console=hvc0", tag),
		SharePath: rootfs,
		ShareTag:  tag,
	}, nil
}
