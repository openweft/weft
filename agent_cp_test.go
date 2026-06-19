//go:build darwin

package weft

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	drivers "github.com/openweft/weft-drivers"
	agent "github.com/openweft/weft/agent"
)

// TestAgentControlPlane_RegisterHost confirms the type-shim:
// agent.HostRegistration arrives at the Adapter as a
// RegisterHostSpec with every field transferred.
func TestAgentControlPlane_RegisterHost(t *testing.T) {
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(stateDir, factory).(*Adapter)
	cp := a.AsControlPlane()

	uuid, err := cp.RegisterHost(context.Background(), agent.HostRegistration{
		UUID:           "test-host-uuid",
		Hostname:       "compute-test",
		AZ:             "dc1",
		Endpoint:       "agent:8443",
		Hypervisor:     "apple-vz",
		Architecture:   "arm64",
		NetworkTypes:   []string{"nat", "mesh"},
		VolumeBackends: []string{"file"},
		Properties:     map[string]string{"gpu": "h100"},
	})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	if uuid != "test-host-uuid" {
		t.Errorf("returned UUID = %q, want test-host-uuid", uuid)
	}
	h, ok := a.HostByUUID("test-host-uuid")
	if !ok {
		t.Fatal("host not in registry after RegisterHost")
	}
	if h.Hostname != "compute-test" || h.AZ != "dc1" || h.Hypervisor != "apple-vz" {
		t.Errorf("host fields not transferred: %+v", h)
	}
	if len(h.NetworkTypes) != 2 || h.Properties["gpu"] != "h100" {
		t.Errorf("network types / properties not transferred: %+v", h)
	}
}

// TestAgentControlPlane_AttachDrivers + Heartbeat covers the
// other two methods. AttachDrivers should land in the
// dispatch table; Heartbeat should bump LastSeenAt.
func TestAgentControlPlane_AttachDriversAndHeartbeat(t *testing.T) {
	a := newAdapterForVMTest(t)
	cp := a.AsControlPlane()

	_, _ = cp.RegisterHost(context.Background(), agent.HostRegistration{
		UUID: "remote-uuid", Hostname: "remote",
	})

	fake := &fakeHypervisor{hostUUID: "remote-uuid"}
	if err := cp.AttachDrivers(context.Background(), "remote-uuid", agent.DriverHandles{
		Hypervisor: fake,
	}); err != nil {
		t.Fatalf("AttachDrivers: %v", err)
	}
	hyp, err := a.HypervisorOn("remote-uuid")
	if err != nil {
		t.Fatalf("HypervisorOn after AttachDrivers: %v", err)
	}
	// Sanity: the attached driver is the one we passed.
	if err := hyp.CreateVM(context.Background(), drivers.VMSpec{UUID: "/tmp/vm-x"}); err != nil {
		t.Fatalf("CreateVM via attached driver: %v", err)
	}
	if len(fake.createdAt) != 1 || fake.createdAt[0] != "/tmp/vm-x" {
		t.Errorf("attached driver didn't receive the call: %+v", fake.createdAt)
	}

	// Heartbeat advances LastSeenAt.
	before, _ := a.HostByUUID("remote-uuid")
	time.Sleep(2 * time.Millisecond)
	if err := cp.Heartbeat(context.Background(), "remote-uuid"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	after, _ := a.HostByUUID("remote-uuid")
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Errorf("LastSeenAt did not advance after Heartbeat")
	}
}

// TestAgentEmbedded_EndToEnd is the showcase: spin up an
// embedded weft-agent inside this weft-control process, watch it
// register + attach via AsControlPlane(), and verify a
// HypervisorOn(agent.HostUUID) call routes to the agent's
// Bundle.
//
// This is the canonical single-process integration path: same
// agent.Agent code that will eventually drive a remote
// weft-agent binary over gRPC; today, the ControlPlane is the
// in-process Adapter shim.
func TestAgentEmbedded_EndToEnd(t *testing.T) {
	// Set up weft-control.
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(stateDir, factory).(*Adapter)

	// Agent uses its own state dir (separate from weft-control's).
	// This mirrors the multi-process future: agent owns
	// /var/lib/weft-agent, control owns /var/lib/weft.
	agentStateDir := filepath.Join(t.TempDir(), "agent-state")
	agent, err := agent.New(agent.Options{
		StateDir: agentStateDir,
		// Avoid the os.Hostname() collision with weft-control's
		// self-registration in the same process — embedded
		// integration test only.
		Hostname:          "embedded-agent-test",
		AZ:                "dc1",
		Endpoint:          "embedded.test:0",
		HeartbeatInterval: time.Hour, // disabled for this test
		ControlPlane:      a.AsControlPlane(),
		// Inject an in-process driver set so the embedded agent doesn't launch
		// the external weft-driver-* plugin (this single-process integration
		// test only cares about the dispatch wiring).
		LocalHandles: &agent.DriverHandles{
			Hypervisor: seamHypervisor{},
			Network:    seamNetwork{},
			Volume:     seamVolume{},
			Image:      seamImage{},
		},
	})
	if err != nil {
		t.Fatalf("New agent: %v", err)
	}
	if err := agent.Start(context.Background()); err != nil {
		t.Fatalf("Agent.Start: %v", err)
	}
	defer agent.Stop(context.Background())

	// The agent registered itself in the Host registry under
	// its persisted UUID.
	if _, ok := a.HostByUUID(agent.HostUUID()); !ok {
		t.Fatalf("agent UUID %q not in host registry", agent.HostUUID())
	}
	// The dispatch table now has an entry for the agent.
	hyp, err := a.HypervisorOn(agent.HostUUID())
	if err != nil {
		t.Fatalf("HypervisorOn(agent): %v", err)
	}
	// Calls route to the agent's Handles.Hypervisor — same
	// object reference proves the wiring.
	if hyp != agent.Handles().Hypervisor {
		t.Errorf("HypervisorOn(agent) returned a different driver than agent.Handles().Hypervisor")
	}

	// Sanity: the local self-registered host is STILL there.
	// The embedded agent is a separate host entry, not a
	// replacement. Single-process weft-control + embedded agent
	// = two host registrations against the same physical host
	// (one for weft-control itself, one for the agent).
	if _, ok := a.HostByUUID(a.localHostUUID()); !ok {
		t.Errorf("local self-registered host disappeared after agent.Start")
	}
}
