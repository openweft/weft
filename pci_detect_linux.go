//go:build linux

package weft

// pci_detect_linux.go is the Linux body of `detectPCI`. It walks
// /sys/bus/pci/devices/*/{vendor,device,driver} and returns every
// endpoint the agent can see, EXCEPT the ones already enumerated
// as GPUs (vendor 0x10de) — those live in Host.GPUs and surface
// through the GPU dimension, not the generic PCI surface.
//
// Today the walk is **minimal** : it returns every non-NVIDIA PCI
// endpoint along with whatever driver the kernel has bound to it
// (typically "vfio-pci" once the operator has unbound the native
// driver). The scheduler doesn't filter by driver — operators are
// expected to declare in cluster.hcl which (vendor:device) tuples
// they intend to passthrough, the scheduler just confirms presence.
//
// Per `coverage_policy` the detector stays CGo-free : sysfs reads
// via os/io, no shell-outs. Detection errors degrade to "log +
// return what we have so far" so a single unreadable device entry
// doesn't take a passthrough host's registration down.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const pciSysfsRoot = "/sys/bus/pci/devices"

func detectPCIImpl() []PCIDevice {
	devs, err := detectPCIFromSysfs(pciSysfsRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: detectPCI sysfs walk: %v\n", err)
	}
	return devs
}

// detectPCIFromSysfs walks /sys/bus/pci/devices/<BDF>/ entries and
// builds the PCIDevice slice. Exported-ish (lowercase) so the test
// can feed in a hand-rolled tmpfs fixture without touching the real
// /sys.
//
// Skips NVIDIA cards (vendor 0x10de) — those belong on the GPU
// surface ; if we returned them here too they'd double-book against
// Host.GPUs and operators reading `weft host info` would see the
// same H200 listed twice.
//
// Output is sorted by BDF so the slice is stable across runs —
// operators reading `weft host info` get the same order on every
// reboot, and the driver layer's "Nth device" semantic stays
// deterministic.
func detectPCIFromSysfs(pciRoot string) ([]PCIDevice, error) {
	matches, err := filepath.Glob(filepath.Join(pciRoot, "*"))
	if err != nil {
		return nil, fmt.Errorf("glob %s/*: %w", pciRoot, err)
	}
	var out []PCIDevice
	for _, dev := range matches {
		bdf := filepath.Base(dev)
		if !looksLikeBDF(bdf) {
			// Not a BDF-shaped entry — sysfs sometimes carries
			// sibling files we can ignore.
			continue
		}
		vendor, err := readPCIID(filepath.Join(dev, "vendor"))
		if err != nil {
			continue
		}
		if strings.EqualFold(vendor, "10de") {
			// NVIDIA — already on the GPU surface.
			continue
		}
		device, err := readPCIID(filepath.Join(dev, "device"))
		if err != nil {
			continue
		}
		driver := readPCIDriver(filepath.Join(dev, "driver"))
		out = append(out, PCIDevice{
			BDF:      bdf,
			VendorID: vendor,
			DeviceID: device,
			Driver:   driver,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BDF < out[j].BDF })
	return out, nil
}

// readPCIID reads one sysfs ID attribute and returns the
// lowercase 4-hex form ("0x10de" → "10de"). PCI IDs are always
// 4 hex digits + a "0x" prefix in sysfs ; we strip both for the
// canonical on-the-wire form.
func readPCIID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if s == "" {
		return "", fmt.Errorf("empty ID at %s", path)
	}
	return s, nil
}

// readPCIDriver resolves the `driver` symlink under a sysfs PCI
// device to the bound driver's name (the last path component).
// Returns "" when the device has no driver bound (the symlink is
// absent then — sysfs doesn't emit a placeholder).
func readPCIDriver(driverLink string) string {
	target, err := os.Readlink(driverLink)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}
