package main

import (
	"context"
	"testing"

	agentv1 "github.com/openweft/weft-proto/agentv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	weft "github.com/openweft/weft"
)

// TestAgentControlPlane_RegisterAgent covers the lifecycle RPC :
// translate HostRegistration → RegisterHostSpec, return the assigned
// UUID, idempotent re-register.
func TestAgentControlPlane_RegisterAgent(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	s := &agentControlPlaneServer{adp: adp}

	resp, err := s.RegisterAgent(context.Background(), &agentv1.RegisterAgentRequest{
		Registration: &agentv1.HostRegistration{
			Uuid:         "host-uuid-1",
			Hostname:     "h1",
			Az:           "dc1",
			Rack:         "r1",
			Hypervisor:   "qemu",
			Architecture: "arm64",
			Properties:   map[string]string{"tier": "edge"},
		},
	})
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if resp.AssignedUuid != "host-uuid-1" {
		t.Errorf("assigned = %q, want host-uuid-1", resp.AssignedUuid)
	}

	// Re-register with same UUID + new placement field : idempotent +
	// new value wins via the existing RegisterHost semantics.
	resp2, err := s.RegisterAgent(context.Background(), &agentv1.RegisterAgentRequest{
		Registration: &agentv1.HostRegistration{
			Uuid: "host-uuid-1", Hostname: "h1",
			Az: "dc2", Rack: "r9", Hypervisor: "qemu", Architecture: "arm64",
		},
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if resp2.AssignedUuid != "host-uuid-1" {
		t.Errorf("re-register uuid changed : got %q", resp2.AssignedUuid)
	}
	h, ok := adp.HostByUUID("host-uuid-1")
	if !ok || h.AZ != "dc2" || h.Rack != "r9" {
		t.Errorf("re-register didn't update AZ/Rack : %+v", h)
	}
}

// TestAgentControlPlane_RegisterAgent_EmptyRequestRejected pins the
// InvalidArgument contract.
func TestAgentControlPlane_RegisterAgent_EmptyRequestRejected(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	s := &agentControlPlaneServer{adp: adp}
	_, err := s.RegisterAgent(context.Background(), &agentv1.RegisterAgentRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

// TestAgentControlPlane_Heartbeat covers the heartbeat RPC against
// a registered host + the NotFound case.
func TestAgentControlPlane_Heartbeat(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	s := &agentControlPlaneServer{adp: adp}
	// Register first.
	if _, err := s.RegisterAgent(context.Background(), &agentv1.RegisterAgentRequest{
		Registration: &agentv1.HostRegistration{Uuid: "h", Hostname: "h", Hypervisor: "qemu", Architecture: "arm64"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(context.Background(), &agentv1.HeartbeatRequest{HostUuid: "h"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// Unknown host : NotFound.
	_, err := s.Heartbeat(context.Background(), &agentv1.HeartbeatRequest{HostUuid: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown host : got %v, want NotFound", err)
	}
	// Empty arg : InvalidArgument.
	_, err = s.Heartbeat(context.Background(), &agentv1.HeartbeatRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty arg : got %v, want InvalidArgument", err)
	}
}
