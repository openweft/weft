package weft

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStorage_LoadAbsentReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStorage(filepath.Join(dir, "missing.hcl"))
	blob, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if blob != nil {
		t.Errorf("expected nil blob for absent file, got %q", blob)
	}
}

func TestFileStorage_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStorage(filepath.Join(dir, "reg.hcl"))
	const payload = `# vzd registry
project "abc-123" {
  name = "demo"
}
`
	if err := s.Save(context.Background(), []byte(payload)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	blob, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(blob) != payload {
		t.Errorf("round-trip mismatch: got %q, want %q", blob, payload)
	}
}

func TestFileStorage_SaveAtomic(t *testing.T) {
	// After Save returns, no leftover .tmp file should be visible.
	dir := t.TempDir()
	path := filepath.Join(dir, "reg.hcl")
	s := NewFileStorage(path)
	if err := s.Save(context.Background(), []byte("data")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file leaked after Save; stat err = %v", err)
	}
}

func TestPathInDir(t *testing.T) {
	got := PathInDir("/var/lib/vzd/vms", "projects")
	want := "/var/lib/vzd/vms/.projects.hcl"
	if got != want {
		t.Errorf("PathInDir = %q, want %q", got, want)
	}
}

func TestFileStorage_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStorage(filepath.Join(dir, "reg.hcl"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Load with canceled ctx: err = %v, want context.Canceled", err)
	}
	if err := s.Save(ctx, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Save with canceled ctx: err = %v, want context.Canceled", err)
	}
}

// ---------- MemStorage ----------

func TestMemStorage_EmptyLoadReturnsNilNil(t *testing.T) {
	s := NewMemStorage()
	blob, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if blob != nil {
		t.Errorf("expected nil blob for empty Mem, got %q", blob)
	}
}

func TestMemStorage_SeedAndIndependence(t *testing.T) {
	// NewMemStorageWith copies the seed so the caller's slice is independent.
	seed := []byte("hello")
	s := NewMemStorageWith(seed)
	seed[0] = 'X' // mutate caller-side
	blob, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "hello" {
		t.Errorf("seed mutation leaked: got %q, want %q", blob, "hello")
	}
}

func TestMemStorage_SaveCopiesInput(t *testing.T) {
	s := NewMemStorage()
	buf := []byte("first")
	if err := s.Save(context.Background(), buf); err != nil {
		t.Fatal(err)
	}
	buf[0] = 'X'
	blob, _ := s.Load(context.Background())
	if string(blob) != "first" {
		t.Errorf("Save didn't copy: got %q, want %q", blob, "first")
	}
}

// ---------- EtcdStorage (real client, no-server cases) ----------

// TestEtcdStorage_NeedsEndpoints pins the validation rule: empty
// Endpoints is a programmer error, not a deferred dial failure.
func TestEtcdStorage_NeedsEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := NewEtcdStorage(ctx, EtcdConfig{KeyPrefix: "/vzd/test/"}, "projects"); err == nil {
		t.Fatal("NewEtcdStorage with no endpoints should fail")
	}
}

// TestEtcdStorage_KeyPrefixApplied checks that the key formed
// inside NewEtcdStorageWithClient honours KeyPrefix + name, which
// is what the factory in cmd/weft relies on for every registry.
func TestEtcdStorageWithClient_KeyFormat(t *testing.T) {
	// Use a nil client — Key() doesn't dereference it.
	s := NewEtcdStorageWithClient(nil, "/vzd/test/", "projects")
	if got := s.Key(); got != "/vzd/test/projects" {
		t.Errorf("Key = %q, want /vzd/test/projects", got)
	}
	// Close on a no-client storage is a no-op.
	if err := s.Close(); err != nil {
		t.Errorf("Close on non-owned no-client storage: %v", err)
	}
}

// ---------- Storage interface satisfaction ----------

// Compile-time check that all three implementations satisfy Storage.
var (
	_ Storage = (*FileStorage)(nil)
	_ Storage = (*MemStorage)(nil)
	_ Storage = (*EtcdStorage)(nil)
)

// ---------- projectRegistry × MemStorage round-trip ----------

func TestProjectRegistry_RoundTripViaMemStorage(t *testing.T) {
	storage := NewMemStorage()
	reg, err := loadProjectRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if len(reg.byUUID) != 0 {
		t.Fatalf("fresh registry should be empty, has %d entries", len(reg.byUUID))
	}
	// Create a project; saveLocked persists via MemStorage.
	p, created, err := reg.getOrCreate("team-alpha")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	if !created {
		t.Fatal("first getOrCreate should report created=true")
	}
	if p.Name != "team-alpha" || p.UUID == "" {
		t.Fatalf("bad project: %+v", p)
	}
	// Re-load via a fresh registry — same Storage — and verify the
	// project survived the persist+reload cycle.
	reg2, err := loadProjectRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(p.UUID)
	if !ok {
		t.Fatalf("reload missed project %s", p.UUID)
	}
	if got.Name != "team-alpha" {
		t.Errorf("reload name = %q, want team-alpha", got.Name)
	}
}
