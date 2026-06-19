package weft

// agent_cp.go implements `agent.ControlPlane` against the
// in-process Adapter. This is the single-process integration
// target: weft-control runs an embedded weft-agent inside the
// same binary; the agent registers + heartbeats by calling
// these wrappers directly instead of going over gRPC.
//
// Pattern: type-conversion shim. Each method translates the
// agent-side value type (agent.HostRegistration,
// agent.DriverHandles) into the weft-control-side type
// (RegisterHostSpec, HostHandle) and delegates to the existing
// Adapter methods. No new business logic — just plumbing.
//
// When the gRPC transport lands, a generated server stub
// implements agent.ControlPlane the same way, calling
// Adapter methods through the RPC handler. The agent code
// stays identical.
//
// See [[weft-driver-registry-split]] for the broader design
// and pkg/openweft/weft/agent/README.md for the agent shape.

import (
	"context"

	agent "github.com/openweft/weft/agent"
)

// AsControlPlane returns a `agent.ControlPlane` view of this
// Adapter. The returned value retains a pointer to `a`, so
// callers should be ok with the agent + adapter sharing the
// same lifetime (typical: agent embedded in weft-control).
func (a *Adapter) AsControlPlane() agent.ControlPlane {
	return adapterControlPlane{a: a}
}

// adapterControlPlane is the thin shim. Value receivers
// throughout — the type is tiny and we don't mutate it.
type adapterControlPlane struct {
	a *Adapter
}

// RegisterHost translates the agent-side HostRegistration into
// weft.RegisterHostSpec + calls a.RegisterHost. Returns the
// assigned UUID (which equals reg.UUID — the Adapter's
// RegisterHost honours pre-set UUIDs for the agent-restart
// case).
func (cp adapterControlPlane) RegisterHost(ctx context.Context, reg agent.HostRegistration) (string, error) {
	spec := RegisterHostSpec{
		UUID:           reg.UUID,
		Hostname:       reg.Hostname,
		AZ:             reg.AZ,
		Endpoint:       reg.Endpoint,
		Hypervisor:     reg.Hypervisor,
		Architecture:   reg.Architecture,
		Drivers:        translateAgentDrivers(reg.Drivers),
		NetworkTypes:   reg.NetworkTypes,
		VolumeBackends: reg.VolumeBackends,
		Properties:     reg.Properties,
	}
	h, err := cp.a.RegisterHost(spec)
	if err != nil {
		return "", err
	}
	return h.UUID, nil
}

// translateAgentDrivers converts the agent-side capability list
// (agent.HostDriverCapability) into the registry-side HostDriver
// shape. Same fields ; the type split is the agent / weft import
// boundary (agent must not import weft).
func translateAgentDrivers(src []agent.HostDriverCapability) []HostDriver {
	if len(src) == 0 {
		return nil
	}
	out := make([]HostDriver, len(src))
	for i, d := range src {
		out[i] = HostDriver{
			Kind:   d.Kind,
			Arches: append([]string(nil), d.Arches...),
		}
	}
	return out
}

// AttachDrivers translates DriverHandles into HostHandle and
// installs it in the Adapter's dispatch table. Single-plugin path.
func (cp adapterControlPlane) AttachDrivers(ctx context.Context, hostUUID string, h agent.DriverHandles) error {
	return cp.a.RegisterHostHandle(hostUUID, &HostHandle{
		Hypervisor: h.Hypervisor,
		Network:    h.Network,
		Volume:     h.Volume,
		Image:      h.Image,
	})
}

// AttachDriverSet translates the per-kind map the agent built in
// multi-plugin mode into a *HostHandle set and installs it under
// hostUUID. Used by Apple Silicon agents running VZ + QEMU.
//
// The primary entry (vz before qemu in stable order) is also
// mirrored into the single-entry dispatch table by
// RegisterHostHandleSet so transitional dispatch paths that don't
// yet know about arch keep working.
func (cp adapterControlPlane) AttachDriverSet(ctx context.Context, hostUUID string, set map[string]agent.DriverHandles) error {
	if len(set) == 0 {
		return agent.ErrAttachSetUnsupported
	}
	stored := make(map[string]*HostHandle, len(set))
	for kind, h := range set {
		stored[kind] = &HostHandle{
			Hypervisor: h.Hypervisor,
			Network:    h.Network,
			Volume:     h.Volume,
			Image:      h.Image,
		}
	}
	return cp.a.RegisterHostHandleSet(hostUUID, stored)
}

// Heartbeat forwards to the Adapter's existing HeartbeatHost.
func (cp adapterControlPlane) Heartbeat(ctx context.Context, hostUUID string) error {
	return cp.a.HeartbeatHost(hostUUID)
}
