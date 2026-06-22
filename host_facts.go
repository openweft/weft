package weft

import "runtime"

// runtimeNumCPU is a thin wrapper around runtime.NumCPU so unit
// tests can stub the value when needed. Cheap ; no syscall.
func runtimeNumCPU() int { return runtime.NumCPU() }

// host_facts.go is the platform-agnostic facade the agent uses to
// collect OS / kernel / network / storage facts at register +
// heartbeat time. Linux carries the real implementation in
// host_facts_linux.go ; every other platform (darwin / *BSD dev
// hosts) falls through to the stubs at the bottom of this file.
//
// Each helper is best-effort : a /proc parse failure returns the
// zero value rather than an error, so a partially-degraded host
// still registers (the operator sees "(unknown)" on the missing
// facet ; sibling fields populate normally).

// HostFacts bundles every fact-collection call into one struct
// so callers stitch them into RegisterHostSpec in one line.
// Empty fields are legal — collection failures degrade silently.
type HostFacts struct {
	OSID              string
	OSVersion         string
	OSPretty          string
	KernelVersion     string
	NetworkInterfaces []NetworkInterface
	StorageMounts     []StorageMount
	CPUCount          int
	MemoryMiB         int64
}

// CollectHostFacts is the public entry point. Linux populates every
// field ; other platforms return a zero-valued struct. Safe to call
// from any goroutine ; cheap (~10ms on a typical 8-NIC Linux host).
func CollectHostFacts() HostFacts {
	return HostFacts{
		OSID:              collectOSRelease("ID"),
		OSVersion:         collectOSRelease("VERSION_ID"),
		OSPretty:          collectOSRelease("PRETTY_NAME"),
		KernelVersion:     collectKernelVersion(),
		NetworkInterfaces: collectNetworkInterfaces(),
		StorageMounts:     collectStorageMounts(),
		CPUCount:          runtimeNumCPU(),
		MemoryMiB:         collectMemoryMiB(),
	}
}

// cloneNetworkInterfaces is the deep-copy used everywhere the
// registry stashes interfaces from a spec. nil-in → nil-out keeps
// the JSON omitempty contract intact.
func cloneNetworkInterfaces(src []NetworkInterface) []NetworkInterface {
	if len(src) == 0 {
		return nil
	}
	out := make([]NetworkInterface, len(src))
	for i, n := range src {
		out[i] = NetworkInterface{
			Name:          n.Name,
			MAC:           n.MAC,
			IPv4CIDRs:     append([]string(nil), n.IPv4CIDRs...),
			IPv6CIDRs:     append([]string(nil), n.IPv6CIDRs...),
			LinkSpeedMbps: n.LinkSpeedMbps,
			MTU:           n.MTU,
			OperState:     n.OperState,
		}
	}
	return out
}

// cloneStorageMounts mirrors cloneNetworkInterfaces. nil-in → nil-out.
func cloneStorageMounts(src []StorageMount) []StorageMount {
	if len(src) == 0 {
		return nil
	}
	out := make([]StorageMount, len(src))
	copy(out, src)
	return out
}
