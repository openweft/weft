package weft

import (
	"context"
	"strings"
	"testing"
)

// TestGenerateProjectUserNKey pins the NATS-conformant shape:
// seed starts with "SU" (S = seed, U = user prefix), pubkey
// starts with "U", and the pubkey round-trips from the seed.
func TestGenerateProjectUserNKey(t *testing.T) {
	nk, err := generateProjectUserNKey()
	if err != nil {
		t.Fatalf("generateProjectUserNKey: %v", err)
	}
	if !strings.HasPrefix(nk.Seed, "SU") {
		t.Errorf("seed should start with SU (user-seed prefix), got %q", nk.Seed[:2])
	}
	if !strings.HasPrefix(nk.Public, "U") {
		t.Errorf("pubkey should start with U (user prefix), got %q", nk.Public[:1])
	}
	pub, err := publicKeyFromSeed(nk.Seed)
	if err != nil {
		t.Fatalf("publicKeyFromSeed: %v", err)
	}
	if pub != nk.Public {
		t.Errorf("pubkey from seed mismatch:\n got: %q\nwant: %q", pub, nk.Public)
	}
}

// TestEnsureNATSUserSeed_LazyMint covers the registry-level
// idempotency: first call mints + persists, second call returns
// the same seed without touching Storage; a freshly-loaded
// registry observes the persisted seed.
func TestEnsureNATSUserSeed_LazyMint(t *testing.T) {
	storage := NewMemStorage()
	reg, err := loadProjectRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("load fresh registry: %v", err)
	}
	p, _, err := reg.getOrCreate("alpha")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	if p.NATSUserSeed != "" {
		t.Fatalf("fresh project should have no seed yet, got %q", p.NATSUserSeed)
	}

	seed1, err := reg.ensureNATSUserSeed(p.UUID)
	if err != nil {
		t.Fatalf("ensureNATSUserSeed first call: %v", err)
	}
	if !strings.HasPrefix(seed1, "SU") {
		t.Errorf("seed should start with SU, got %q", seed1)
	}

	seed2, err := reg.ensureNATSUserSeed(p.UUID)
	if err != nil {
		t.Fatalf("ensureNATSUserSeed second call: %v", err)
	}
	if seed2 != seed1 {
		t.Errorf("second call should be idempotent\n got: %q\nwant: %q", seed2, seed1)
	}

	// Force a registry reload — the seed must survive the round-trip.
	reg2, err := loadProjectRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(p.UUID)
	if !ok {
		t.Fatalf("project %s vanished from reloaded registry", p.UUID)
	}
	if got.NATSUserSeed != seed1 {
		t.Errorf("seed lost across reload\n got: %q\nwant: %q", got.NATSUserSeed, seed1)
	}
}

// TestEnsureNATSUserSeed_UnknownProject pins the negative path:
// asking for a seed on a project that doesn't exist must error
// rather than silently mint one for a phantom UUID.
func TestEnsureNATSUserSeed_UnknownProject(t *testing.T) {
	storage := NewMemStorage()
	reg, err := loadProjectRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if _, err := reg.ensureNATSUserSeed("00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("expected error for unknown project UUID")
	}
}
