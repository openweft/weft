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
		Properties:     map[string]string{"gpu": "none"},
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
		t.Errorf("duplicate hostname should be rejected (prior host still live)")
	}
}

// TestHostRegistry_HostnameTakeoverWhenPriorIsStale covers the
// re-imaged-host scenario : same hostname re-registers with a new
// UUID after the prior agent died or its state/ was wiped. The
// prior entry is stale (LastSeenAt > staleHostTakeoverAge or
// State==Down) so the new registration must evict + replace.
func TestHostRegistry_HostnameTakeoverWhenPriorIsStale(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	// Plant a stale prior entry : Active state but LastSeenAt
	// further in the past than the takeover threshold.
	prior, err := reg.register(RegisterHostSpec{UUID: "prior-uuid", Hostname: "c-01"})
	if err != nil {
		t.Fatal(err)
	}
	prior.LastSeenAt = time.Now().Add(-2 * staleHostTakeoverAge).UTC()
	reg.byUUID["prior-uuid"] = prior
	// Re-register with a new UUID under the same hostname.
	fresh, err := reg.register(RegisterHostSpec{UUID: "fresh-uuid", Hostname: "c-01"})
	if err != nil {
		t.Fatalf("takeover should succeed when prior is stale, got %v", err)
	}
	if fresh.UUID != "fresh-uuid" {
		t.Errorf("takeover UUID = %s, want fresh-uuid", fresh.UUID)
	}
	if _, ok := reg.byUUID["prior-uuid"]; ok {
		t.Errorf("prior-uuid entry should have been evicted")
	}
	if got := reg.nameIdx["c-01"]; got != "fresh-uuid" {
		t.Errorf("nameIdx[c-01] = %s, want fresh-uuid", got)
	}
}

// TestHostRegistry_IdempotentReRegisterPreservesAZRack covers the
// same gotcha as the takeover preservation rule, but for the
// idempotent-by-UUID re-register path : a hot agent restart should
// NOT blank AZ/Rack when WEFT_AZ/WEFT_RACK aren't exported. The
// fix is the don't-clobber-on-empty pattern the AKName + WGPublicKey
// branches already use.
func TestHostRegistry_IdempotentReRegisterPreservesAZRack(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	_, err := reg.register(RegisterHostSpec{
		UUID:     "stable-uuid",
		Hostname: "c-01",
		AZ:       "dc1",
		Rack:     "r2",
		Properties: map[string]string{
			"tier": "edge",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same UUID, same hostname, but empty placement fields — the
	// `nohup weft agent &` without env vars scenario.
	got, err := reg.register(RegisterHostSpec{
		UUID:     "stable-uuid",
		Hostname: "c-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AZ != "dc1" {
		t.Errorf("AZ = %q, want dc1 (preserved on re-register)", got.AZ)
	}
	if got.Rack != "r2" {
		t.Errorf("Rack = %q, want r2 (preserved on re-register)", got.Rack)
	}
	if got.Properties["tier"] != "edge" {
		t.Errorf("Properties = %v, want {tier:edge} preserved", got.Properties)
	}
}

// TestHostRegistry_HostnameTakeoverPreservesPlacementMetadata covers
// the gotcha surfaced live : when selfRegister fires without
// WEFT_AZ/WEFT_RACK in the env, the agent passes empty placement
// fields ; the takeover policy must carry over the prior entry's
// AZ/Rack/Properties so an unintentional relaunch doesn't blank
// the cluster.hcl-time placement metadata.
func TestHostRegistry_HostnameTakeoverPreservesPlacementMetadata(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	prior, err := reg.register(RegisterHostSpec{
		UUID:       "prior-uuid",
		Hostname:   "c-01",
		AZ:         "dc1",
		Rack:       "r2",
		Properties: map[string]string{"tier": "edge", "role": "compute"},
	})
	if err != nil {
		t.Fatal(err)
	}
	prior.LastSeenAt = time.Now().Add(-2 * staleHostTakeoverAge).UTC()
	reg.byUUID["prior-uuid"] = prior

	// Fresh registration carries an empty AZ/Rack/Properties — the
	// classic `nohup weft agent &` without env vars set.
	fresh, err := reg.register(RegisterHostSpec{UUID: "fresh-uuid", Hostname: "c-01"})
	if err != nil {
		t.Fatalf("takeover should succeed, got %v", err)
	}
	if fresh.AZ != "dc1" {
		t.Errorf("AZ = %q, want dc1 (preserved from prior)", fresh.AZ)
	}
	if fresh.Rack != "r2" {
		t.Errorf("Rack = %q, want r2 (preserved from prior)", fresh.Rack)
	}
	if fresh.Properties["tier"] != "edge" || fresh.Properties["role"] != "compute" {
		t.Errorf("Properties = %v, want {tier:edge,role:compute} preserved", fresh.Properties)
	}
}

// TestHostRegistry_HostnameTakeoverExplicitFieldsOverride covers the
// other half of the preservation rule : when the new spec DOES carry
// non-empty AZ/Rack, those win over the prior's. Operators
// explicitly moving a host between racks shouldn't be defeated by
// the carry-over.
func TestHostRegistry_HostnameTakeoverExplicitFieldsOverride(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	prior, err := reg.register(RegisterHostSpec{
		UUID:     "prior-uuid",
		Hostname: "c-01",
		AZ:       "dc1",
		Rack:     "r2",
	})
	if err != nil {
		t.Fatal(err)
	}
	prior.LastSeenAt = time.Now().Add(-2 * staleHostTakeoverAge).UTC()
	reg.byUUID["prior-uuid"] = prior

	fresh, err := reg.register(RegisterHostSpec{
		UUID: "fresh-uuid", Hostname: "c-01",
		AZ:   "dc2", // operator explicitly relocates
		Rack: "r1",
	})
	if err != nil {
		t.Fatalf("takeover should succeed, got %v", err)
	}
	if fresh.AZ != "dc2" || fresh.Rack != "r1" {
		t.Errorf("explicit AZ/Rack overridden by preservation : got %s/%s, want dc2/r1", fresh.AZ, fresh.Rack)
	}
}

// TestHostRegistry_HostnameTakeoverRefusedWhenPriorIsFresh : if
// the prior entry's heartbeat is recent, takeover must refuse — a
// silent hijack of a live agent's identity is the failure mode we
// surface as an error.
func TestHostRegistry_HostnameTakeoverRefusedWhenPriorIsFresh(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	if _, err := reg.register(RegisterHostSpec{UUID: "prior-uuid", Hostname: "c-01"}); err != nil {
		t.Fatal(err)
	}
	_, err := reg.register(RegisterHostSpec{UUID: "fresh-uuid", Hostname: "c-01"})
	if err == nil {
		t.Errorf("takeover should be refused when prior is still fresh")
	}
}

// TestHostRegistry_HostnameTakeoverWhenPriorIsDown : Down state is
// a positive signal (dispatch-session sweeper flipped the host)
// so takeover applies regardless of LastSeenAt age.
func TestHostRegistry_HostnameTakeoverWhenPriorIsDown(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	prior, err := reg.register(RegisterHostSpec{UUID: "prior-uuid", Hostname: "c-01"})
	if err != nil {
		t.Fatal(err)
	}
	// LastSeenAt is fresh but State has been flipped to Down.
	prior.State = HostStateDown
	reg.byUUID["prior-uuid"] = prior
	fresh, err := reg.register(RegisterHostSpec{UUID: "fresh-uuid", Hostname: "c-01"})
	if err != nil {
		t.Fatalf("takeover should succeed when prior State==Down, got %v", err)
	}
	if fresh.UUID != "fresh-uuid" {
		t.Errorf("takeover UUID = %s, want fresh-uuid", fresh.UUID)
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
		Properties:     map[string]string{"gpu": "h100"},
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

func TestHostRegistry_SetProperties(t *testing.T) {
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
		Properties:     map[string]string{"gpu": "h100", "ssd": "true"},
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
		"properties",
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
