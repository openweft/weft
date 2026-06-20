//go:build linux

package weft

// gpu_detect_linux.go is the Linux body of `detectGPUs`. It walks
// /sys/class/drm/card*/device/ to enumerate NVIDIA cards (vendor
// 0x10de), then enriches the inventory by parsing
// `nvidia-smi --query-gpu=name,memory.total,uuid,mig.mode.current
// --format=csv,noheader` when the binary is on PATH.
//
// Two stages on purpose : the sysfs walk gives us the PCI BDF
// (required by the QEMU driver to emit -device vfio-pci) without
// requiring nvidia-smi to be installed. The shell-out fills in
// the user-facing model name, per-card memory and the MIG mode
// flag — all of which nvidia-smi knows authoritatively.
//
// The output of `nvidia-smi --query-gpu=…` is parsed against a
// hand-rolled CSV reader (encoding/csv handles the format but
// pulls a fat dependency for what is one comma-and-quote-free
// line per card). Tests pin the parser against pre-canned
// strings copied verbatim from real nvidia-smi 545.x stdout so
// future driver upgrades surface as test failures.
//
// Per `coverage_policy` the detector stays CGo-free : sysfs
// reads via os/io, the SMI parse is pure-string. nvidia-smi is
// resolved with exec.LookPath ; absent → log + return the
// sysfs-only inventory (better than nothing). Detection errors
// degrade to "log + return what we have so far" so a single
// flaky card doesn't take a multi-GPU host's registration down.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// pciVendorNVIDIA is the PCI Vendor ID for every NVIDIA card the
// fleet has carried since Riva 128 — kept as a const so the sysfs
// walk reads top-down without a magic literal in the middle of
// the byte parse.
const pciVendorNVIDIA = "0x10de"

// detectGPUsImpl is the Linux body the package-level `detectGPUs`
// delegator calls. Build-tagged sibling at `gpu_detect_other.go`
// returns an empty slice for non-Linux platforms.
func detectGPUsImpl() []GPU {
	gpus, err := detectGPUsFromSysfs("/sys/class/drm")
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: detectGPUs sysfs walk: %v\n", err)
		// Fall through with whatever we collected before the error —
		// a partial inventory is more useful than an empty one when a
		// multi-GPU host has one flaky card.
	}
	if len(gpus) == 0 {
		return nil
	}
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		// nvidia-smi absent (operator opted out, or a freshly-staged
		// driver-less host) — sysfs alone gave us the BDFs ; we lack
		// model name / memory / MIG flag. Return what we have ;
		// canonicalGPUModel(raw="") will warn-and-skip downstream.
		return gpus
	}
	out, err := exec.Command(smi,
		"--query-gpu=name,memory.total,uuid,mig.mode.current",
		"--format=csv,noheader").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: detectGPUs nvidia-smi: %v\n", err)
		return gpus
	}
	enriched, err := enrichWithSMI(gpus, strings.NewReader(string(out)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: detectGPUs enrich: %v\n", err)
		return gpus
	}
	// MIG enumeration : `nvidia-smi -L` lists each MIG instance with its
	// profile + UUID under its parent GPU. enumerateMIGFromSMIL attaches
	// them to the matching card and (per the EITHER/OR invariant) clears
	// the parent's whole-card BDF. Best-effort : a -L failure leaves the
	// whole-card inventory intact rather than failing registration.
	lout, err := exec.Command(smi, "-L").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: detectGPUs nvidia-smi -L: %v\n", err)
		return enriched
	}
	return enumerateMIGFromSMIL(enriched, strings.NewReader(string(lout)))
}

// detectGPUsFromSysfs walks the DRM subsystem looking for PCI
// devices whose vendor matches 0x10de (NVIDIA). One GPU entry per
// matching card. Public-ish (lowercase) for the test which feeds
// in a hand-rolled tmpfs fixture.
//
// Cards are returned sorted by PCI BDF so the slice is stable
// across runs — operators reading `weft host info` get the same
// order on every reboot, and the QEMU driver's "Nth GPU" semantic
// stays deterministic.
func detectGPUsFromSysfs(drmRoot string) ([]GPU, error) {
	matches, err := filepath.Glob(filepath.Join(drmRoot, "card*", "device"))
	if err != nil {
		return nil, fmt.Errorf("glob %s/card*/device: %w", drmRoot, err)
	}
	var out []GPU
	seen := make(map[string]bool) // BDF dedup — multiple cardN entries can resolve to one device
	for _, dev := range matches {
		vendor, err := readTrimmed(filepath.Join(dev, "vendor"))
		if err != nil {
			continue // not every card*/device has a vendor (render nodes etc.)
		}
		if !strings.EqualFold(vendor, pciVendorNVIDIA) {
			continue
		}
		bdf, err := bdfFromSysfsDevice(dev)
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft: detectGPUs: skip %s: %v\n", dev, err)
			continue
		}
		if seen[bdf] {
			continue
		}
		seen[bdf] = true
		out = append(out, GPU{
			Vendor: GPUVendorNVIDIA,
			PCIBDF: bdf,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PCIBDF < out[j].PCIBDF })
	return out, nil
}

// bdfFromSysfsDevice resolves a `…/card*/device` symlink to the
// underlying `…/devices/pci…/<BDF>` and returns the BDF segment.
// sysfs guarantees the last path component of the resolved target
// is the canonical "0000:bb:dd.f" form.
func bdfFromSysfsDevice(devPath string) (string, error) {
	target, err := os.Readlink(devPath)
	if err != nil {
		// Some sysfs layouts expose the device dir directly (not via
		// a symlink). Fall back to the dir's basename — its `uevent`
		// will carry PCI_SLOT_NAME with the same string.
		return bdfFromUevent(filepath.Join(devPath, "uevent"))
	}
	// `target` looks like "../../../0000:65:00.0" — Base() peels off
	// the BDF segment.
	bdf := filepath.Base(target)
	if !looksLikeBDF(bdf) {
		// Symlink resolves to something unexpected — try uevent as a
		// belt-and-braces fallback.
		return bdfFromUevent(filepath.Join(devPath, "uevent"))
	}
	return bdf, nil
}

// bdfFromUevent parses sysfs uevent files of the form
// `PCI_SLOT_NAME=0000:65:00.0` to recover the BDF. Tolerant of
// the line ordering kernels do not stabilise — just scan top-down
// for the first match.
func bdfFromUevent(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if eq := strings.SplitN(line, "=", 2); len(eq) == 2 && eq[0] == "PCI_SLOT_NAME" {
			if looksLikeBDF(eq[1]) {
				return eq[1], nil
			}
		}
	}
	return "", fmt.Errorf("uevent %s: PCI_SLOT_NAME not found", path)
}

// looksLikeBDF is a cheap "does this string look like 0000:65:00.0"
// check. Not strict (won't reject odd domains) but tight enough to
// catch sysfs-layout drift early — the BDF format has been stable
// since 2.6 so a non-match is almost certainly a parse bug.
func looksLikeBDF(s string) bool {
	// Domain:bus:device.function — 4:2:2.1 hex chars.
	if len(s) < len("0000:00:00.0") {
		return false
	}
	if s[4] != ':' || s[7] != ':' || s[10] != '.' {
		return false
	}
	return true
}

// readTrimmed reads a sysfs attribute (one short value per file)
// and trims the trailing newline kernels always append.
func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// enrichWithSMI merges nvidia-smi CSV output into the sysfs-walked
// GPU slice. The match key is **order** : nvidia-smi enumerates
// devices in the same PCI order the kernel exposes them, so the
// Nth CSV row enriches the Nth sysfs entry. When the row count
// disagrees we fall back to "enrich what we can, return the rest
// verbatim" rather than fail registration — operators get the
// sysfs-only inventory and a stderr log they can debug.
//
// CSV shape (one card per line) :
//
//	"NVIDIA H200, 143771 MiB, GPU-abc…, Disabled"
//
// `name` is freeform vendor copy — `canonicalGPUModel` normalises
// it onto the SchedulingRule axis form. `memory.total` is in MiB
// with a trailing unit ; we strip the unit and convert to GiB.
// `mig.mode.current` is "Enabled"/"Disabled" — only Enabled means
// the operator has actually opted into MIG ; `MIGCapable` here is
// best-effort and the canonical-model lookup overrides it for
// known SKUs (H200 = always MIG-capable, RTX 6000 Ada never).
func enrichWithSMI(base []GPU, r io.Reader) ([]GPU, error) {
	rows, err := parseSMIRows(r)
	if err != nil {
		return base, err
	}
	out := make([]GPU, len(base))
	copy(out, base)
	for i, row := range rows {
		if i >= len(out) {
			fmt.Fprintf(os.Stderr,
				"weft: detectGPUs: nvidia-smi reported %d rows but sysfs only %d — extras ignored\n",
				len(rows), len(out))
			break
		}
		model, migCap, defMem, known := canonicalGPUModel(row.name)
		if !known {
			// Warn-and-keep : we still record the raw model so operators
			// see "this is what the card reported" rather than nothing.
			fmt.Fprintf(os.Stderr,
				"weft: detectGPUs: unknown NVIDIA SKU %q at %s — keeping raw model\n",
				row.name, out[i].PCIBDF)
		}
		out[i].Model = model
		// SMI's mig.mode.current trumps the static map (operator may
		// have disabled MIG on an H200) — but only when it actually
		// reports Enabled. Disabled is the factory default ; the SKU
		// table still records *capability*, distinct from current mode.
		out[i].MIGCapable = migCap
		if strings.EqualFold(row.migMode, "Enabled") {
			out[i].MIGCapable = true
		}
		if row.memMiB > 0 {
			out[i].MemoryGiB = row.memMiB / 1024
		} else if defMem > 0 {
			out[i].MemoryGiB = defMem
		}
	}
	return out, nil
}

// smiRow is one parsed nvidia-smi CSV row. UUID is captured for
// debugging / future claim-table keys but not yet surfaced on the
// GPU struct (would couple the inventory to a runtime identifier
// that changes across MIG re-partitioning).
type smiRow struct {
	name    string
	memMiB  int
	uuid    string
	migMode string
}

// parseSMIRows is a deliberately hand-rolled CSV parser. The
// nvidia-smi `--format=csv,noheader` output is comma-separated,
// no embedded quotes / commas in cells, one row per line. Pulling
// encoding/csv would handle this but adds a ~3kLoC dependency for
// what is a 20-line split.
func parseSMIRows(r io.Reader) ([]smiRow, error) {
	sc := bufio.NewScanner(r)
	// Single rows are ~80 bytes ; allow up to 8 KiB to absorb future
	// columns without thinking about it.
	sc.Buffer(make([]byte, 0, 1024), 8*1024)
	var rows []smiRow
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			return rows, fmt.Errorf("nvidia-smi row %q: want 4 fields, got %d", line, len(fields))
		}
		mem, err := parseMiBField(strings.TrimSpace(fields[1]))
		if err != nil {
			return rows, fmt.Errorf("nvidia-smi row %q: memory.total: %w", line, err)
		}
		rows = append(rows, smiRow{
			name:    strings.TrimSpace(fields[0]),
			memMiB:  mem,
			uuid:    strings.TrimSpace(fields[2]),
			migMode: strings.TrimSpace(fields[3]),
		})
	}
	if err := sc.Err(); err != nil {
		return rows, err
	}
	return rows, nil
}

// parseMiBField turns a "143771 MiB" cell into the integer 143771.
// nvidia-smi always emits the unit suffix when --format=csv is
// used without ,nounits ; strip it and parse the leading int.
func parseMiBField(s string) (int, error) {
	// Strip the unit suffix if present — keep the parse permissive so
	// a future ,nounits flip doesn't break us.
	for _, suffix := range []string{" MiB", " MB", "MiB", "MB"} {
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSpace(strings.TrimSuffix(s, suffix))
			break
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	return n, nil
}
