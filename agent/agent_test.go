//go:build darwin

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	drivers "github.com/openweft/weft-drivers"
)

// recordingCP captures every call the agent makes. Used to
// assert the agent's startup + heartbeat sequence without a
// real control plane.
type recordingCP struct {
	mu             sync.Mutex
	registered     []HostRegistration
	attachedFor    []string
	attachedHandle map[string]DriverHandles
	// attachedSetFor + attachedSet mirror the singular fields above
	// for the multi-plugin path. Populated by AttachDriverSet.
	attachedSetFor []string
	attachedSet    map[string]map[string]DriverHandles
	heartbeats     int
	registerErr    error
	attachErr      error
	// attachSetErr controls AttachDriverSet's return value :
	// - nil (default unset) → return ErrAttachSetUnsupported so the
	//   agent falls back to AttachDrivers (current single-plugin tests
	//   keep working without modification).
	// - any other value → returned as-is. Set to nil explicitly via
	//   `cp.attachSetErr = nil` if the test wants AttachDriverSet to
	//   succeed AND verifying via a non-default sentinel.
	attachSetErr error
}

func newRecordingCP() *recordingCP {
	return &recordingCP{
		attachedHandle: make(map[string]DriverHandles),
		attachedSet:    make(map[string]map[string]DriverHandles),
	}
}

func (cp *recordingCP) RegisterHost(ctx context.Context, reg HostRegistration) (string, error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.registered = append(cp.registered, reg)
	if cp.registerErr != nil {
		return "", cp.registerErr
	}
	return reg.UUID, nil
}

func (cp *recordingCP) AttachDrivers(ctx context.Context, hostUUID string, h DriverHandles) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.attachedFor = append(cp.attachedFor, hostUUID)
	cp.attachedHandle[hostUUID] = h
	return cp.attachErr
}

// AttachDriverSet returns ErrAttachSetUnsupported by default so the
// agent falls back to AttachDrivers with the primary entry — keeps
// the existing single-plugin tests passing without changes. Tests
// that exercise the multi-plugin path can flip this via attachSetErr
// (nil = record the call as accepted).
func (cp *recordingCP) AttachDriverSet(ctx context.Context, hostUUID string, set map[string]DriverHandles) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.attachedSetFor = append(cp.attachedSetFor, hostUUID)
	cp.attachedSet[hostUUID] = set
	if cp.attachSetErr != nil {
		return cp.attachSetErr
	}
	return ErrAttachSetUnsupported
}

func (cp *recordingCP) Heartbeat(ctx context.Context, hostUUID string) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.heartbeats++
	return nil
}

func (cp *recordingCP) heartbeatCount() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.heartbeats
}

func TestNew_RejectsMissingFields(t *testing.T) {
	if _, err := New(Options{StateDir: "/tmp"}); err == nil {
		t.Errorf("missing ControlPlane should be rejected")
	}
	if _, err := New(Options{ControlPlane: newRecordingCP()}); err == nil {
		t.Errorf("missing StateDir should be rejected")
	}
}

// TestStart_RegistersAndAttaches exercises the happy path: the
// agent registers + attaches its drivers in one call.
func TestStart_RegistersAndAttaches(t *testing.T) {
	cp := newRecordingCP()
	a, err := New(Options{
		StateDir:          t.TempDir(),
		AZ:                "us-east-1a",
		Endpoint:          "agent-1.internal:8443",
		HeartbeatInterval: 50 * time.Millisecond, // fast so the heartbeat fires in the test
		ControlPlane:      cp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if a.HostUUID() == "" {
		t.Errorf("HostUUID should be set after Start")
	}
	if len(cp.registered) != 1 {
		t.Fatalf("expected exactly 1 RegisterHost call, got %d", len(cp.registered))
	}
	reg := cp.registered[0]
	if reg.UUID == "" || reg.UUID != a.HostUUID() {
		t.Errorf("RegisterHost UUID mismatch: reg=%q agent=%q", reg.UUID, a.HostUUID())
	}
	if reg.AZ != "us-east-1a" {
		t.Errorf("AZ not propagated: %q", reg.AZ)
	}
	if reg.Endpoint != "agent-1.internal:8443" {
		t.Errorf("Endpoint not propagated: %q", reg.Endpoint)
	}
	if reg.Hypervisor != "apple-vz" {
		t.Errorf("Hypervisor not set: %q", reg.Hypervisor)
	}
	if len(cp.attachedFor) != 1 || cp.attachedFor[0] != a.HostUUID() {
		t.Errorf("AttachDrivers not called with host UUID: %v", cp.attachedFor)
	}
	h := cp.attachedHandle[a.HostUUID()]
	if h.Hypervisor == nil || h.Network == nil || h.Volume == nil || h.Image == nil {
		t.Errorf("attached handles missing a driver: %+v", h)
	}
	// The attached Hypervisor must be the same instance the
	// agent's Bundle exposes — proves the dispatch wiring
	// points at the right object.
	var _ drivers.HypervisorDriver = h.Hypervisor
	if h.Hypervisor != a.Handles().Hypervisor {
		t.Errorf("attached Hypervisor != agent's Handles.Hypervisor")
	}
}

// TestStart_RegisterErrorPropagates: a control plane that
// refuses Register should kill the agent's Start with the same
// error visible to the caller.
func TestStart_RegisterErrorPropagates(t *testing.T) {
	cp := newRecordingCP()
	cp.registerErr = errors.New("control plane is down")
	a, err := New(Options{StateDir: t.TempDir(), ControlPlane: cp})
	if err != nil {
		t.Fatal(err)
	}
	err = a.Start(context.Background())
	if err == nil {
		t.Fatal("Start should propagate Register failure")
	}
	if !strings.Contains(err.Error(), "control plane is down") {
		t.Errorf("error should surface CP error, got: %v", err)
	}
}

// TestStart_AttachDriversErrorPropagates: same idea for the
// AttachDrivers step.
func TestStart_AttachDriversErrorPropagates(t *testing.T) {
	cp := newRecordingCP()
	cp.attachErr = errors.New("dispatch table full")
	a, err := New(Options{StateDir: t.TempDir(), ControlPlane: cp})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("Start should propagate Attach failure")
	}
}

// TestHeartbeatLoop_Fires: the heartbeat goroutine ticks at
// the configured interval. Use a very short interval + wait a
// few intervals + assert > 0 heartbeats.
func TestHeartbeatLoop_Fires(t *testing.T) {
	cp := newRecordingCP()
	a, err := New(Options{
		StateDir:          t.TempDir(),
		HeartbeatInterval: 20 * time.Millisecond,
		ControlPlane:      cp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Wait ~5 intervals.
	time.Sleep(120 * time.Millisecond)
	_ = a.Stop(context.Background())
	if got := cp.heartbeatCount(); got < 2 {
		t.Errorf("expected >= 2 heartbeats, got %d", got)
	}
}

// TestUUIDPersistsAcrossInstances: two consecutive Agents
// against the same StateDir resolve to the same host UUID.
// Mirrors the weft-control invariant — restart safety.
func TestUUIDPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	cp1 := newRecordingCP()
	a1, _ := New(Options{StateDir: dir, ControlPlane: cp1, HeartbeatInterval: time.Hour})
	if err := a1.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = a1.Stop(context.Background())

	cp2 := newRecordingCP()
	a2, _ := New(Options{StateDir: dir, ControlPlane: cp2, HeartbeatInterval: time.Hour})
	if err := a2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = a2.Stop(context.Background())

	if a1.HostUUID() != a2.HostUUID() {
		t.Errorf("UUID changed across restart: %q → %q", a1.HostUUID(), a2.HostUUID())
	}
	// Sanity: the UUID file is on disk.
	b, err := os.ReadFile(filepath.Join(dir, "host-uuid"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != a1.HostUUID() {
		t.Errorf("host-uuid file mismatch")
	}
}

// TestStop_Idempotent: Stop is safe to call multiple times.
func TestStop_Idempotent(t *testing.T) {
	cp := newRecordingCP()
	a, _ := New(Options{StateDir: t.TempDir(), ControlPlane: cp, HeartbeatInterval: time.Hour})
	_ = a.Start(context.Background())
	_ = a.Stop(context.Background())
	_ = a.Stop(context.Background()) // second call must not panic
}
