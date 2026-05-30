package agent

// grpc_cp.go is the gRPC-backed implementation of the
// ControlPlane interface. The agent dials a remote `weft
// agent --server` and calls the Host registry RPCs to register
// itself, heartbeat, and (in a future commit) attach its
// driver handles.
//
// Today this implements RegisterHost + Heartbeat against the
// existing `weft-proto` RPCs (`RegisterHost`, `HeartbeatHost`).
// AttachDrivers is a no-op with a logged warning : the
// reverse-direction RPC needed for the control plane to call
// back into the agent's drivers requires a bidirectional
// stream that isn't wired yet. Schedulers can still pick this
// host (it shows up in the Host registry) ; VM dispatch to it
// will start working when that stream lands.
//
// Wire shape (today) :
//
//   weft agent --client --control-plane=URL
//        │
//        │ gRPC over Unix socket / TCP / SSH
//        ▼
//   weft agent --server (running elsewhere)
//        │
//        │ Adapter.RegisterHost / HeartbeatHost
//        ▼
//   Host registry (etcd or file)
//
// Future : bidirectional stream for driver dispatch.

import (
	"context"
	"fmt"
	"log"

	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// HostRegistryClient is the slim slice of the gRPC client this
// stub needs : the two Host-registry methods. Defining it here
// (rather than taking the full `weftv1.WeftAgentClient`) keeps
// tests trivial — mock just these two — and avoids dragging
// dozens of unrelated methods into the agent's import surface.
// The real generated client satisfies this interface structurally.
type HostRegistryClient interface {
	RegisterHost(ctx context.Context, in *weftv1.RegisterHostRequest, opts ...grpc.CallOption) (*weftv1.RegisterHostResponse, error)
	HeartbeatHost(ctx context.Context, in *weftv1.HeartbeatHostRequest, opts ...grpc.CallOption) (*weftv1.HeartbeatHostResponse, error)
}

// NewGRPCControlPlane wraps an existing gRPC client into a
// ControlPlane. The agent treats it identically to the
// in-process Adapter shim (see weft.AsControlPlane).
//
// `logger` is optional — pass nil to silence the deferred
// AttachDrivers warning. Used so operators see what's
// not-yet-wired without having to grep the source.
func NewGRPCControlPlane(c HostRegistryClient, logger *log.Logger) ControlPlane {
	return &grpcControlPlane{c: c, logger: logger}
}

type grpcControlPlane struct {
	c      HostRegistryClient
	logger *log.Logger
}

// RegisterHost calls the server-side `RegisterHost` RPC. The
// returned UUID matches `reg.UUID` for the agent-restart case ;
// the server mints a fresh one when reg.UUID is empty.
func (g *grpcControlPlane) RegisterHost(ctx context.Context, reg HostRegistration) (string, error) {
	req := &weftv1.RegisterHostRequest{
		Uuid:           reg.UUID,
		Hostname:       reg.Hostname,
		Az:             reg.AZ,
		Rack:           reg.Rack,
		Endpoint:       reg.Endpoint,
		Hypervisor:     reg.Hypervisor,
		Architecture:   reg.Architecture,
		NetworkTypes:   reg.NetworkTypes,
		VolumeBackends: reg.VolumeBackends,
		Labels:         reg.Labels,
	}
	resp, err := g.c.RegisterHost(ctx, req)
	if err != nil {
		return "", fmt.Errorf("grpc RegisterHost: %w", err)
	}
	if resp.Host == nil {
		return "", fmt.Errorf("grpc RegisterHost: server returned empty host")
	}
	return resp.Host.Uuid, nil
}

// AttachDrivers is a placeholder until the bidirectional
// stream is wired. Logged once per call so the operator sees
// the deferred work in the agent's log stream.
func (g *grpcControlPlane) AttachDrivers(_ context.Context, hostUUID string, _ DriverHandles) error {
	if g.logger != nil {
		g.logger.Printf("grpc control-plane: AttachDrivers(%s) is a no-op until the bidirectional driver-dispatch stream is wired ; this host is visible to the scheduler but VM dispatch will land on the control-plane-local host until then", hostUUID)
	}
	return nil
}

// Heartbeat calls the server-side `HeartbeatHost` RPC. Errors
// bubble up so the agent's heartbeat goroutine can decide
// whether to back off + retry.
func (g *grpcControlPlane) Heartbeat(ctx context.Context, hostUUID string) error {
	_, err := g.c.HeartbeatHost(ctx, &weftv1.HeartbeatHostRequest{Uuid: hostUUID})
	if err != nil {
		return fmt.Errorf("grpc HeartbeatHost: %w", err)
	}
	return nil
}
