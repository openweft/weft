//go:build darwin

package agent

import (
	"context"
	"errors"
	"testing"

	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// fakeGRPCClient records what the stub sent + returns
// programmable responses. Implements the HostRegistryClient
// surface so the gRPC stub takes it directly.
type fakeGRPCClient struct {
	regCalls  []*weftv1.RegisterHostRequest
	hbCalls   []*weftv1.HeartbeatHostRequest
	regResp   *weftv1.RegisterHostResponse
	regErr    error
	hbErr     error
}

func (f *fakeGRPCClient) RegisterHost(_ context.Context, in *weftv1.RegisterHostRequest, _ ...grpc.CallOption) (*weftv1.RegisterHostResponse, error) {
	f.regCalls = append(f.regCalls, in)
	if f.regErr != nil {
		return nil, f.regErr
	}
	return f.regResp, nil
}

func (f *fakeGRPCClient) HeartbeatHost(_ context.Context, in *weftv1.HeartbeatHostRequest, _ ...grpc.CallOption) (*weftv1.HeartbeatHostResponse, error) {
	f.hbCalls = append(f.hbCalls, in)
	if f.hbErr != nil {
		return nil, f.hbErr
	}
	return &weftv1.HeartbeatHostResponse{}, nil
}

// TestGRPCControlPlane_RegisterHost pins the agent→server
// translation : every HostRegistration field is faithfully
// copied into RegisterHostRequest. Catches drift between the
// agent's struct shape and the proto.
func TestGRPCControlPlane_RegisterHost(t *testing.T) {
	fake := &fakeGRPCClient{
		regResp: &weftv1.RegisterHostResponse{Host: &weftv1.HostInfo{Uuid: "h-abc"}},
	}
	cp := NewGRPCControlPlane(fake, nil)

	reg := HostRegistration{
		UUID:           "agent-uuid",
		Hostname:       "node-1",
		AZ:             "dc1",
		Rack:           "r3",
		Endpoint:       "10.0.0.5:7777",
		Hypervisor:     "apple-vz",
		Architecture:   "arm64",
		NetworkTypes:   []string{"nat", "bridged"},
		VolumeBackends: []string{"file"},
		Labels:         map[string]string{"gpu": "h100"},
	}
	got, err := cp.RegisterHost(context.Background(), reg)
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	if got != "h-abc" {
		t.Errorf("returned uuid = %q, want h-abc", got)
	}
	if len(fake.regCalls) != 1 {
		t.Fatalf("got %d RegisterHost calls, want 1", len(fake.regCalls))
	}
	r := fake.regCalls[0]
	if r.Uuid != reg.UUID || r.Hostname != reg.Hostname || r.Az != reg.AZ ||
		r.Rack != reg.Rack || r.Endpoint != reg.Endpoint ||
		r.Hypervisor != reg.Hypervisor || r.Architecture != reg.Architecture {
		t.Errorf("scalar field mismatch: %+v", r)
	}
	if len(r.NetworkTypes) != 2 || r.NetworkTypes[0] != "nat" || r.NetworkTypes[1] != "bridged" {
		t.Errorf("NetworkTypes = %v", r.NetworkTypes)
	}
	if r.Labels["gpu"] != "h100" {
		t.Errorf("Labels missing gpu=h100: %+v", r.Labels)
	}
}

// TestGRPCControlPlane_RegisterHost_EmptyResponse pins the
// defensive error path : a server that returns a nil Host is a
// protocol violation ; the stub surfaces it as an error rather
// than returning an empty UUID.
func TestGRPCControlPlane_RegisterHost_EmptyResponse(t *testing.T) {
	fake := &fakeGRPCClient{regResp: &weftv1.RegisterHostResponse{Host: nil}}
	cp := NewGRPCControlPlane(fake, nil)
	_, err := cp.RegisterHost(context.Background(), HostRegistration{Hostname: "h"})
	if err == nil {
		t.Fatal("expected error for nil-Host response")
	}
}

// TestGRPCControlPlane_Heartbeat pins the keepalive path.
func TestGRPCControlPlane_Heartbeat(t *testing.T) {
	fake := &fakeGRPCClient{}
	cp := NewGRPCControlPlane(fake, nil)
	if err := cp.Heartbeat(context.Background(), "h-abc"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if len(fake.hbCalls) != 1 || fake.hbCalls[0].Uuid != "h-abc" {
		t.Errorf("Heartbeat calls: %+v", fake.hbCalls)
	}
}

// TestGRPCControlPlane_Heartbeat_ErrorBubblesUp pins error
// propagation — the agent's heartbeat goroutine needs to see
// transport failures so it can back off + retry.
func TestGRPCControlPlane_Heartbeat_ErrorBubblesUp(t *testing.T) {
	fake := &fakeGRPCClient{hbErr: errors.New("network unreachable")}
	cp := NewGRPCControlPlane(fake, nil)
	err := cp.Heartbeat(context.Background(), "h-abc")
	if err == nil {
		t.Fatal("expected error to bubble up")
	}
}

// TestGRPCControlPlane_AttachDrivers_AcceptsWireSide pins the
// production semantics : AttachDrivers over gRPC accepts the call
// and returns nil because the wire-side advertisement happens via
// RegisterHost.Drivers and dispatch flows over AgentDispatch ; the
// handles themselves are local in-process pointers that can't and
// don't cross the boundary.
func TestGRPCControlPlane_AttachDrivers_AcceptsWireSide(t *testing.T) {
	cp := NewGRPCControlPlane(&fakeGRPCClient{}, nil)
	if err := cp.AttachDrivers(context.Background(), "h-abc", DriverHandles{}); err != nil {
		t.Errorf("AttachDrivers should accept the call, got: %v", err)
	}
}

// TestGRPCControlPlane_AttachDriverSet_AcceptsMultiKind pins the
// multi-plugin variant : the per-kind handle map is local
// metadata, so the wire layer accepts it and returns nil. The
// previous ErrAttachSetUnsupported path was a scaffold that forced
// the agent to fall back to AttachDrivers ; now that we
// acknowledge per-kind is already covered by RegisterHost.Drivers
// + AgentDispatch, the fallback is unnecessary.
func TestGRPCControlPlane_AttachDriverSet_AcceptsMultiKind(t *testing.T) {
	cp := NewGRPCControlPlane(&fakeGRPCClient{}, nil)
	set := map[string]DriverHandles{
		"vz":   {},
		"qemu": {},
	}
	if err := cp.AttachDriverSet(context.Background(), "h-multi", set); err != nil {
		t.Errorf("AttachDriverSet should accept the multi-kind call, got: %v", err)
	}
}

// TestGRPCControlPlane_AttachDriverSet_EmptyMap covers the
// boundary case : an agent with zero driver plugins (rare ; means
// the operator misconfigured the cluster). We still return nil —
// the host registers itself as having no driver kinds, the
// scheduler simply won't dispatch anything to it.
func TestGRPCControlPlane_AttachDriverSet_EmptyMap(t *testing.T) {
	cp := NewGRPCControlPlane(&fakeGRPCClient{}, nil)
	if err := cp.AttachDriverSet(context.Background(), "h-empty", map[string]DriverHandles{}); err != nil {
		t.Errorf("AttachDriverSet should tolerate an empty map, got: %v", err)
	}
}
