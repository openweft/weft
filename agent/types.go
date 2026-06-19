package agent

// types.go declares the protobuf-friendly value types crossed
// between weft-agent and weft-control via the ControlPlane
// interface. Kept in this module on purpose — weft-agent must
// not import the weft control-plane module (see README); each
// side carries its own copy of these tiny records.
//
// Mirror types: weft.RegisterHostSpec, weft.HostHandle, and the
// weft.Host returned by RegisterHost all have direct counterparts
// here. The control-plane adapter that implements ControlPlane
// is responsible for translating between the two shapes.

import (
	drivers "github.com/openweft/weft-drivers"
)

// HostRegistration is what the agent sends at startup. The
// control plane uses this to populate its Host registry entry
// for this agent's host.
type HostRegistration struct {
	// UUID is the agent's persisted host-uuid. The control
	// plane uses this as the key under which to register the
	// driver handle — idempotent on agent restart.
	UUID string
	// Hostname / AZ / Rack tag the physical machine. Hostname
	// must be unique cluster-wide; AZ + Rack drive placement
	// rules per [[weft-placement-rules]].
	Hostname string
	AZ       string
	Rack     string
	// Endpoint is the agent's gRPC listener (host:port). Empty
	// when the agent is embedded in weft-control's process.
	Endpoint string
	// Hypervisor / Architecture describe the host's PRIMARY driver
	// and its native arch — kept for backward-compat with
	// single-driver agents + clients that don't yet know about
	// Drivers. When Drivers is non-empty these are the first entry
	// of that list.
	Hypervisor   string
	Architecture string
	// Drivers is the full capability list — one entry per
	// weft-driver-<kind> subprocess this agent has launched, with
	// the set of guest archs each can run. A bare-metal Apple
	// Silicon machine running both VZ (native arm64) and QEMU
	// (foreign archs for cross-builds) registers two entries here.
	// Empty Drivers means "use Hypervisor / Architecture as a
	// single-driver registration".
	Drivers []HostDriverCapability
	// NetworkTypes / VolumeBackends are the capability lists
	// the scheduler matches against.
	NetworkTypes   []string
	VolumeBackends []string
	// Properties are operator-set free-form tags ("gpu=h100",
	// "ssd=true"). Used by ScheduleRequest's property selectors.
	Properties map[string]string
}

// HostDriverCapability is one driver subprocess running on a host,
// with the set of guest architectures it can launch. Mirrors
// cluster.HostDriver (HCL side) and weft.HostDriver (registry side)
// on the agent ↔ control-plane wire.
type HostDriverCapability struct {
	Kind   string   // "vz" | "qemu"
	Arches []string // "arm64" | "amd64" | "riscv64" | "loongarch64"
}

// DriverHandles bundles the four driver interfaces the agent
// exposes. The control plane stores them in its dispatch table
// under the agent's HostUUID.
//
// In the in-process integration case these are the concrete
// *builtin.Hypervisor / *builtin.Network / *builtin.Volume /
// *builtin.Image returned by `builtin.New(...)`. In the gRPC
// case (future) they're client stubs that implement the same
// driver interfaces by RPC-ing over the wire to the agent
// process.
type DriverHandles struct {
	Hypervisor drivers.HypervisorDriver
	Network    drivers.NetworkDriver
	Volume     drivers.VolumeDriver
	Image      drivers.ImageDriver
}
