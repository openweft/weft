package weft

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openweft/weft/wgcoord"
)

// TestEnsureHostWGKey_StableAcrossCalls mints a host WG key on first call and
// confirms the second call returns the SAME public key (re-derived from the
// persisted private key, not freshly minted).
func TestEnsureHostWGKey_StableAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	pub1, err := EnsureHostWGKey(dir)
	if err != nil {
		t.Fatalf("EnsureHostWGKey: %v", err)
	}
	if pub1 == "" {
		t.Fatal("first call returned empty public key")
	}
	pub2, err := EnsureHostWGKey(dir)
	if err != nil {
		t.Fatalf("EnsureHostWGKey (reload): %v", err)
	}
	if pub1 != pub2 {
		t.Errorf("public key changed across calls: %q vs %q", pub1, pub2)
	}
}

// TestEnsureHostWGKey_PrivateKeyPersisted0600 confirms the PRIVATE key is the
// thing on disk (0o600) and that the returned public key derives from it —
// i.e. the private half is what's persisted and the public half is what
// leaves the function.
func TestEnsureHostWGKey_PrivateKeyPersisted0600(t *testing.T) {
	dir := t.TempDir()
	pub, err := EnsureHostWGKey(dir)
	if err != nil {
		t.Fatalf("EnsureHostWGKey: %v", err)
	}
	path := filepath.Join(dir, hostWGKeyFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file perm = %o, want 600", perm)
	}
	priv, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	derived, err := wgcoord.PublicFromPrivate(string(priv))
	if err != nil {
		t.Fatalf("PublicFromPrivate(persisted): %v", err)
	}
	if derived != pub {
		t.Errorf("persisted file is not the private key for the returned public key: %q != %q", derived, pub)
	}
	// The returned value must NOT equal the on-disk material (public != private).
	if string(priv) == pub {
		t.Error("returned key equals the persisted private key — the private half must never be returned")
	}
}

// TestEnsureHostWGKey_CorruptKeyFile surfaces a decode error rather than
// silently re-minting (which would change the host's identity).
func TestEnsureHostWGKey_CorruptKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, hostWGKeyFile), []byte("not-base64-!!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureHostWGKey(dir); err == nil {
		t.Fatal("expected error for corrupt key file, got nil")
	}
}

// TestHostWGIndexFromUUID_StableAndInRange checks the derived index is
// deterministic and within the usable host range.
func TestHostWGIndexFromUUID_StableAndInRange(t *testing.T) {
	uuid := "abc-123-def-456"
	got1 := hostWGIndexFromUUID(uuid)
	got2 := hostWGIndexFromUUID(uuid)
	if got1 != got2 {
		t.Errorf("index not deterministic: %d vs %d", got1, got2)
	}
	if got1 < 1 || got1 > maxHostIndex {
		t.Errorf("index %d out of range [1,%d]", got1, maxHostIndex)
	}
	// Different UUIDs should (here) produce indices that the allocator can map.
	alloc, err := wgcoord.NewAllocator("10.9.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alloc.Host(got1); err != nil {
		t.Errorf("allocator rejects derived index %d: %v", got1, err)
	}
}
