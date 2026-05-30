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
	// Hypervisor / Architecture describe what this host can run.
	Hypervisor   string
	Architecture string
	// NetworkTypes / VolumeBackends are the capability lists
	// the scheduler matches against.
	NetworkTypes   []string
	VolumeBackends []string
	// Labels are operator-set free-form tags ("gpu=h100",
	// "ssd=true"). Used by ScheduleRequest's label selectors.
	Labels map[string]string
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
