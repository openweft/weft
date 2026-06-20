package weft

// gpu_mig.go holds the platform-neutral parser that turns `nvidia-smi -L`
// output into MIG-instance inventory. It lives in an untagged file (not
// gpu_detect_linux.go) on purpose : the parse is pure string-munging with
// no syscall surface, so keeping it here lets it be unit-tested on every
// GOOS instead of only Linux. The Linux detector (gpu_detect_linux.go)
// runs `nvidia-smi -L` and hands the reader to enumerateMIGFromSMIL.
//
// Why -L (and not `mig -lgi`/`-lci`) : a single `nvidia-smi -L` call lists
// every MIG device with its profile and CUDA/MIG UUID, grouped under its
// parent GPU — enough to populate the allocatable MIGInstance unit (the
// UUID is what the QEMU driver passes as vfio-pci sysfsdev=). The GI/CI
// numeric ids that `mig -lgi`/`-lci` add are diagnostic only and are left
// zero here ; a later refinement can enrich them if a need appears.

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// enumerateMIGFromSMIL parses `nvidia-smi -L` and attaches MIG instances
// to the matching parent card in `base`. Output shape :
//
//	GPU 0: NVIDIA H200 (UUID: GPU-5b1c…)
//	  MIG 1g.18gb     Device  0: (UUID: MIG-9c1e…)
//	  MIG 1g.18gb     Device  1: (UUID: MIG-3a7f…)
//	GPU 1: NVIDIA H200 (UUID: GPU-7d2a…)
//
// The "GPU N" index matches the Nth card in PCI-enumeration order — the
// same ordering enrichWithSMI relies on — so it maps onto base[N].
//
// CRITICAL invariant (docs/operations/gpu-sharing.md) : a card that
// exposes MIG instances reports them INSTEAD of a whole-card resource.
// So when instances are attached, the card's PCIBDF is moved onto each
// instance's ParentBDF and GPU.PCIBDF is CLEARED — guaranteeing a
// whole-card claim and a MIG claim can never target the same silicon
// (the exclusive matcher skips empty-BDF cards for whole-card requests).
//
// Returns a copy ; `base` is not mutated. Cards with no MIG lines are
// passed through verbatim (BDF intact, no instances).
func enumerateMIGFromSMIL(base []GPU, r io.Reader) []GPU {
	out := make([]GPU, len(base))
	copy(out, base)
	// Don't share the backing array of any pre-existing MIGInstances slice.
	for i := range out {
		out[i].MIGInstances = nil
	}

	sc := bufio.NewScanner(r)
	cur := -1 // index into out of the GPU whose block we're inside
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if idx, ok := parseSMILGPUIndex(line); ok {
			cur = idx
			continue
		}
		if cur < 0 || cur >= len(out) {
			continue
		}
		profile, uuid, ok := parseSMILMIGLine(line)
		if !ok {
			continue
		}
		out[cur].MIGInstances = append(out[cur].MIGInstances, MIGInstance{
			ParentBDF: base[cur].PCIBDF, // original BDF — stable across instances
			Profile:   profile,
			UUID:      uuid,
			MemoryGiB: migProfileMemGiB(profile),
		})
		// EITHER/OR invariant : a MIG-mode card is not whole-card-allocatable.
		out[cur].PCIBDF = ""
	}
	return out
}

// parseSMILGPUIndex returns the N from a `GPU N: …` header line. The bool
// is false for any other line, including the indented `MIG …` sub-lines.
func parseSMILGPUIndex(line string) (int, bool) {
	if !strings.HasPrefix(line, "GPU ") {
		return 0, false
	}
	rest := strings.TrimPrefix(line, "GPU ")
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest[:colon]))
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseSMILMIGLine extracts (profile, uuid) from a `MIG <profile> Device
// N: (UUID: MIG-…)` line. ok is false when the line isn't a MIG device
// line or carries no UUID.
func parseSMILMIGLine(line string) (profile, uuid string, ok bool) {
	if !strings.HasPrefix(line, "MIG ") {
		return "", "", false
	}
	fields := strings.Fields(strings.TrimPrefix(line, "MIG "))
	if len(fields) == 0 {
		return "", "", false
	}
	profile = fields[0]
	uuid = extractParenUUID(line)
	if uuid == "" {
		return "", "", false
	}
	return profile, uuid, true
}

// extractParenUUID pulls the value out of a `(UUID: <value>)` clause,
// tolerant of surrounding whitespace. Returns "" when absent.
func extractParenUUID(line string) string {
	i := strings.Index(line, "UUID:")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[i+len("UUID:"):])
	if j := strings.IndexByte(rest, ')'); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// migProfileMemGiB derives the per-slice memory in GiB from a MIG profile
// string : the digits after the dot ("1g.18gb" → 18, "3g.71gb" → 71).
// Tolerant of trailing qualifiers ("1g.18gb+me" → 18). Returns 0 when the
// profile doesn't carry a parseable memory segment.
func migProfileMemGiB(profile string) int {
	dot := strings.IndexByte(profile, '.')
	if dot < 0 {
		return 0
	}
	mem := profile[dot+1:]
	end := 0
	for end < len(mem) && mem[end] >= '0' && mem[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(mem[:end])
	if err != nil {
		return 0
	}
	return n
}
