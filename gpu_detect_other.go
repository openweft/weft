//go:build !linux

package weft

// gpu_detect_other.go is the non-Linux body of `detectGPUs`. NVIDIA
// data-center GPUs only present a sysfs / nvidia-smi surface on
// Linux ; macOS hosts run weft for dev under TCG (no passthrough)
// and Windows isn't a target. Returning nil keeps registration
// alive without pretending to enumerate hardware we can't reach.
//
// Build-tagged so the binary stays CGo-free + cross-compilable on
// every GOOS the rest of weft supports (darwin, linux, freebsd,
// windows). The Linux body lives in `gpu_detect_linux.go`.

func detectGPUsImpl() []GPU {
	return nil
}
