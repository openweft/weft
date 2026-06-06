// Image cache layout helpers.
//
// One image lives at:
//
//	$XDG_DATA_HOME/weft-microvm/images/<refsafe>/
//	├── manifest.json   (raw bytes pulled from the registry)
//	├── config.json     (the image config blob the manifest points at)
//	└── rootfs/         (every layer extracted in order, whiteouts applied)
//	    └── .weft-microvm/config.json   (runtime-style process spec derived from
//	                            the image config; consumed by weft-microvm-init
//	                            after pivot_root)
//
// `refsafe` is the OCI reference rewritten so it's a safe filesystem
// name (slashes and colons become underscores). Two different
// references that resolve to the same manifest will get two cache
// entries — that's fine for v1; dedup-by-digest is a roadmap nicety.

package microvm

import (
	"os"
	"path/filepath"
	"strings"
)

// dataHome returns the per-user data dir per XDG, falling back to
// ~/.local/share on macOS where XDG isn't typically set.
func dataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "share")
	}
	return "/tmp"
}

// dataDir is the weft-microvm per-user data root (kernel, initrd, images,
// pods, …) under the XDG data home. Single source of truth for the namespace
// — renamed from the legacy "weft-microvm" dir when the microVM runtime moved under
// the weft umbrella (repos weft-microvm + weft-microvm-kernel).
func dataDir() string {
	return filepath.Join(dataHome(), "weft-microvm")
}

// imageRoot is the cache directory for one image reference (the dir
// holding manifest.json, config.json, and rootfs/).
func imageRoot(refsafe string) string {
	return filepath.Join(dataDir(), "images", refsafe)
}

// rootfsPath is where the layers get extracted.
func rootfsPath(refsafe string) string {
	return filepath.Join(imageRoot(refsafe), "rootfs")
}

// KernelPath is the on-disk location of the shared microVM kernel binary —
// $XDG_DATA_HOME/weft-microvm/kernel — written by PullKernel and read by the
// agent's RegisterMicroVM. Exported because the agent reads it via the same
// path resolution rule the puller uses.
func KernelPath() string { return filepath.Join(dataDir(), "kernel") }

// PodInitrdPath is the on-disk location of the shared pod-mode initramfs —
// $XDG_DATA_HOME/weft-microvm/pod-initrd — written by PullPodInitrd and
// read by the agent (see weft-microvm/pod.go's locatePodBoot, which expects
// this exact path unless $WEFT_POD_INITRD is set).
func PodInitrdPath() string { return filepath.Join(dataDir(), "pod-initrd") }

// refsafe converts an OCI reference like "alpine:3.21" or
// "ghcr.io/owner/repo:v1.2.3" into a single safe directory name.
// Slashes and colons become underscores; everything else passes
// through. This is deterministic per reference so callers can
// look up a previously-pulled image without re-parsing.
func refsafe(image string) string {
	r := strings.NewReplacer("/", "_", ":", "_")
	return r.Replace(image)
}
