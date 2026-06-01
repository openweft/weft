//go:build !linux

package weft

// pci_detect_other.go is the non-Linux body of `detectPCI`. PCI
// passthrough is a Linux-only path (Apple Virtualization framework
// has no pci passthrough surface ; QEMU on darwin runs under TCG
// without passthrough). Returning nil keeps registration alive on
// darwin / freebsd / windows dev boxes without pretending to
// enumerate hardware we can't reach.
//
// Build-tagged so the binary stays CGo-free + cross-compilable on
// every GOOS the rest of weft supports. The Linux body lives in
// pci_detect_linux.go.

func detectPCIImpl() []PCIDevice {
	return nil
}
