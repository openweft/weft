package weft

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHostRegistry_EmptyOnFreshStorage(t *testing.T) {
	reg, err := loadHostRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadHostRegistry: %v", err)
	}
	if got := len(reg.byUUID); got != 0 {
		t.Errorf("fresh registry has %d entries, want 0", got)
	}
}

func TestHostRegistry_Register(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h, err := reg.register(RegisterHostSpec{
		Hostname:       "compute-01",
		AZ:             "us-east-1a",
		Endpoint:       "compute-01.internal:8443",
		Hypervisor:     "apple-vz",
		Architecture:   "arm64",
		NetworkTypes:   []string{"nat", "bridged", "mesh"},
		VolumeBackends: []string{"file"},
		Labels:         map[string]string{"gpu": "none"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if h.UUID == "" {
		t.Errorf("host should have UUID")
	}
	if h.State != HostStateActive {
		t.Errorf("new host state = %q, want active", h.State)
	}
	if h.LastSeenAt.IsZero() {
		t.Errorf("LastSeenAt should be set on register")
	}
	if got, ok := reg.lookupByUUID(h.UUID); !ok || got.UUID != h.UUID {
		t.Errorf("lookupByUUID failed")
	}
	if got, ok := reg.lookupByHostname("compute-01"); !ok || got.UUID != h.UUID {
		t.Errorf("lookupByHostname failed")
	}
}

func TestHostRegistry_HostnameUnique(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	_, err := reg.register(RegisterHostSpec{Hostname: "c-01"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.register(RegisterHostSpec{Hostname: "c-01"})
	if err == nil {
		t.Errorf("duplicate hostname should be rejected")
	}
}

// TestHostRegistry_IdempotentReRegister covers the
// agent-restart pattern: same UUID + hostname, possibly drifted
// metadata → registry refreshes mutable fields, preserves
// CreatedAt + UUID.
func TestHostRegistry_IdempotentReRegister(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h1, err := reg.register(RegisterHostSpec{
		UUID:           "stable-uuid",
		Hostname:       "c-01",
		AZ:             "us-east-1a",
		Hypervisor:     "apple-vz",
		Architecture:   "arm64",
		NetworkTypes:   []string{"nat"},
		VolumeBackends: []string{"file"},
	})
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if h1.UUID != "stable-uuid" {
		t.Errorf("provided UUID not honored: got %q", h1.UUID)
	}
	firstSeen := h1.LastSeenAt
	created := h1.CreatedAt

	time.Sleep(2 * time.Millisecond)
	// Agent restart with drifted capabilities + a new label.
	h2, err := reg.register(RegisterHostSpec{
		UUID:           "stable-uuid",
		Hostname:       "c-01",
		AZ:             "us-east-1a",
		Hypervisor:     "apple-vz",
		Architecture:   "arm64",
		NetworkTypes:   []string{"nat", "mesh"},
		VolumeBackends: []string{"file", "ceph"},
		Labels:         map[string]string{"gpu": "h100"},
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if h2.UUID != h1.UUID {
		t.Errorf("UUID mutated across re-register: %q → %q", h1.UUID, h2.UUID)
	}
	if h2.CreatedAt != created {
		t.Errorf("CreatedAt mutated across re-register")
	}
	if !h2.LastSeenAt.After(firstSeen) {
		t.Errorf("LastSeenAt didn't advance: %v → %v", firstSeen, h2.LastSeenAt)
	}
	if len(h2.NetworkTypes) != 2 || h2.NetworkTypes[1] != "mesh" {
		t.Errorf("capabilities not refreshed: %v", h2.NetworkTypes)
	}
	if h2.Properties["gpu"] != "h100" {
		t.Errorf("labels not refreshed: %v", h2.Properties)
	}

	// Down → Active on re-register (heartbeat-like revival).
	_ = reg.setState(h1.UUID, HostStateDown)
	h3, _ := reg.register(RegisterHostSpec{UUID: "stable-uuid", Hostname: "c-01"})
	if h3.State != HostStateActive {
		t.Errorf("re-register should revive Down → Active, got %q", h3.State)
	}

	// Hostname mismatch on re-register is rejected — operators
	// must Delete + re-register if the agent's hostname changed.
	if _, err := reg.register(RegisterHostSpec{UUID: "stable-uuid", Hostname: "different"}); err == nil {
		t.Errorf("hostname mismatch on re-register should be rejected")
	}
}

// TestHostRegistry_PreSetUUIDFreshRegister covers the case where
// the caller provides a UUID and no host with that UUID exists
// yet — the registry honors the UUID rather than generating one.
func TestHostRegistry_PreSetUUIDFreshRegister(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h, err := reg.register(RegisterHostSpec{
		UUID:     "explicit-uuid-from-agent",
		Hostname: "c-01",
	})
	if err != nil {
		t.Fatalf("fresh register with pre-set UUID: %v", err)
	}
	if h.UUID != "explicit-uuid-from-agent" {
		t.Errorf("UUID not honored: got %q", h.UUID)
	}
}

func TestHostRegistry_RejectsEmptyHostname(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	if _, err := reg.register(RegisterHostSpec{}); err == nil {
		t.Errorf("empty hostname should be rejected")
	}
}

func TestHostRegistry_Heartbeat(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h, _ := reg.register(RegisterHostSpec{Hostname: "c-01"})
	t0 := h.LastSeenAt
	time.Sleep(2 * time.Millisecond)
	if err := reg.heartbeat(h.UUID); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.lookupByUUID(h.UUID)
	if !got.LastSeenAt.After(t0) {
		t.Errorf("LastSeenAt didn't advance: %v → %v", t0, got.LastSeenAt)
	}
	// Heartbeat reverses Down → Active.
	_ = reg.setState(h.UUID, HostStateDown)
	_ = reg.heartbeat(h.UUID)
	got, _ = reg.lookupByUUID(h.UUID)
	if got.State != HostStateActive {
		t.Errorf("heartbeat should revive Down → Active, got %q", got.State)
	}
	// Draining is preserved across heartbeat.
	_ = reg.setState(h.UUID, HostStateDraining)
	_ = reg.heartbeat(h.UUID)
	got, _ = reg.lookupByUUID(h.UUID)
	if got.State != HostStateDraining {
		t.Errorf("heartbeat should not touch Draining, got %q", got.State)
	}
	// Unknown UUID rejected.
	if err := reg.heartbeat("nope"); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
}

func TestHostRegistry_SetState(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h, _ := reg.register(RegisterHostSpec{Hostname: "c"})
	for _, s := range []HostState{HostStateDraining, HostStateDown, HostStateActive} {
		if err := reg.setState(h.UUID, s); err != nil {
			t.Errorf("setState(%q): %v", s, err)
		}
	}
	// Invalid state rejected.
	if err := reg.setState(h.UUID, HostState("flying")); err == nil {
		t.Errorf("invalid state should be rejected")
	}
	// Empty state rejected.
	if err := reg.setState(h.UUID, ""); err == nil {
		t.Errorf("empty state should be rejected")
	}
	// Unknown UUID rejected.
	if err := reg.setState("nope", HostStateActive); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
}

func TestHostRegistry_SetLabels(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h, _ := reg.register(RegisterHostSpec{Hostname: "c"})
	if err := reg.setProperties(h.UUID, map[string]string{"gpu": "h100", "ssd": "true"}); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.lookupByUUID(h.UUID)
	if got.Properties["gpu"] != "h100" {
		t.Errorf("labels not applied: %v", got.Properties)
	}
	// Clear via nil.
	_ = reg.setProperties(h.UUID, nil)
	got, _ = reg.lookupByUUID(h.UUID)
	if len(got.Properties) != 0 {
		t.Errorf("labels should be cleared: %v", got.Properties)
	}
}

func TestHostRegistry_DeleteRefusesActive(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h, _ := reg.register(RegisterHostSpec{Hostname: "c"})
	if err := reg.delete(h.UUID); err == nil {
		t.Errorf("delete of active host should be refused")
	}
	_ = reg.setState(h.UUID, HostStateDraining)
	if err := reg.delete(h.UUID); err != nil {
		t.Fatalf("delete after drain: %v", err)
	}
	if _, ok := reg.lookupByUUID(h.UUID); ok {
		t.Errorf("host should be gone after delete")
	}
	if _, ok := reg.lookupByHostname("c"); ok {
		t.Errorf("hostname index should be cleaned up")
	}
	if err := reg.delete("nope"); err == nil {
		t.Errorf("delete unknown UUID should be rejected")
	}
}

func TestHostRegistry_ListAndListByAZ(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	_, _ = reg.register(RegisterHostSpec{Hostname: "c-01", AZ: "us-east-1a"})
	_, _ = reg.register(RegisterHostSpec{Hostname: "c-02", AZ: "us-east-1a"})
	_, _ = reg.register(RegisterHostSpec{Hostname: "c-03", AZ: "us-east-1b"})

	all := reg.list()
	if len(all) != 3 {
		t.Fatalf("list size = %d, want 3", len(all))
	}
	// Sorted by AZ then hostname.
	if all[0].Hostname != "c-01" || all[1].Hostname != "c-02" || all[2].Hostname != "c-03" {
		t.Errorf("list order wrong: %v", []string{all[0].Hostname, all[1].Hostname, all[2].Hostname})
	}
	if g := reg.listByAZ("us-east-1a"); len(g) != 2 {
		t.Errorf("listByAZ(1a) = %d, want 2", len(g))
	}
	if g := reg.listByAZ("us-west-1"); len(g) != 0 {
		t.Errorf("listByAZ unknown should be empty")
	}
}

// TestHostRegistry_RoundTripViaStorage confirms HCL encode + decode
// preserve every field, including the labels map and the
// timestamps.
func TestHostRegistry_RoundTripViaStorage(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadHostRegistry(context.Background(), storage)
	h, _ := reg.register(RegisterHostSpec{
		Hostname:       "compute-01",
		AZ:             "us-east-1a",
		Endpoint:       "compute-01.internal:8443",
		Hypervisor:     "apple-vz",
		Architecture:   "arm64",
		NetworkTypes:   []string{"nat", "bridged", "mesh"},
		VolumeBackends: []string{"file"},
		Labels:         map[string]string{"gpu": "h100", "ssd": "true"},
	})
	_ = reg.setState(h.UUID, HostStateDraining)

	blob, _ := storage.Load(context.Background())
	for _, want := range []string{
		"host \"" + h.UUID + "\"",
		"compute-01",
		"us-east-1a",
		"apple-vz",
		"network_types",
		"mesh",
		"labels",
		"h100",
		"draining",
	} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("HCL missing %q: %s", want, blob)
		}
	}

	reg2, err := loadHostRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	got, ok := reg2.lookupByUUID(h.UUID)
	if !ok {
		t.Fatal("host lost on re-load")
	}
	if got.AZ != "us-east-1a" || got.Hypervisor != "apple-vz" || got.Architecture != "arm64" {
		t.Errorf("scalars not preserved: %+v", got)
	}
	if len(got.NetworkTypes) != 3 {
		t.Errorf("network_types not preserved: %v", got.NetworkTypes)
	}
	if got.Properties["gpu"] != "h100" || got.Properties["ssd"] != "true" {
		t.Errorf("labels not preserved: %v", got.Properties)
	}
	if got.State != HostStateDraining {
		t.Errorf("state not preserved: %q", got.State)
	}
	if got.LastSeenAt.IsZero() || got.CreatedAt.IsZero() {
		t.Errorf("timestamps not preserved")
	}
	// hostname index re-built.
	if g, ok := reg2.lookupByHostname("compute-01"); !ok || g.UUID != h.UUID {
		t.Errorf("hostname index didn't survive reload")
	}
}
