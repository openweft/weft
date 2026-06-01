package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func genB64Key(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen : %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func TestJoinIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "weft.hcl")
	pubA := genB64Key(t)
	pubB := genB64Key(t)

	// Empty config → first join.
	fb, err := ReadFileBlock(cfg)
	if err != nil {
		t.Fatalf("read empty : %v", err)
	}
	if fb != nil {
		t.Fatal("missing file must yield nil block")
	}
	fb = &FileBlock{}
	if err := fb.AddPeer("https://peer-a", pubA); err != nil {
		t.Fatalf("AddPeer A : %v", err)
	}
	if err := fb.AddPeer("https://peer-b", pubB); err != nil {
		t.Fatalf("AddPeer B : %v", err)
	}
	if err := WriteFileBlock(cfg, fb); err != nil {
		t.Fatalf("Write : %v", err)
	}
	first, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read first : %v", err)
	}
	// Re-join with the same args → byte-identical file (proof of
	// idempotency, the deterministic sort matters here).
	again, err := ReadFileBlock(cfg)
	if err != nil {
		t.Fatalf("read after first write : %v", err)
	}
	if again == nil {
		t.Fatal("read after write must yield non-nil block")
	}
	if err := again.AddPeer("https://peer-a", pubA); err != nil {
		t.Fatalf("re-AddPeer : %v", err)
	}
	if err := WriteFileBlock(cfg, again); err != nil {
		t.Fatalf("Write again : %v", err)
	}
	second, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read second : %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("not byte-identical on re-join :\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	// Same URL, different pubkey → key rotation, replaces but
	// doesn't duplicate the slice entry.
	pubC := genB64Key(t)
	if err := again.AddPeer("https://peer-a", pubC); err != nil {
		t.Fatalf("rotate : %v", err)
	}
	if len(again.Peers) != 2 {
		t.Fatalf("peer count must stay 2 after rotation, got %d", len(again.Peers))
	}
	if again.PeerKeys["https://peer-a"] != pubC {
		t.Fatal("rotation must replace the pubkey")
	}
}

func TestLeaveAndValidate(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "weft.hcl")
	pubA := genB64Key(t)
	fb := &FileBlock{}
	if err := fb.AddPeer("https://peer-a", pubA); err != nil {
		t.Fatalf("AddPeer : %v", err)
	}
	if err := WriteFileBlock(cfg, fb); err != nil {
		t.Fatalf("Write : %v", err)
	}
	round, err := ReadFileBlock(cfg)
	if err != nil {
		t.Fatalf("Read : %v", err)
	}
	if !round.RemovePeer("https://peer-a") {
		t.Fatal("RemovePeer must succeed")
	}
	if round.RemovePeer("https://peer-a") {
		t.Fatal("RemovePeer twice must be false")
	}
	// IsEmpty must be true after removing the only peer (assuming
	// no other fields were set).
	if !round.IsEmpty() {
		t.Fatal("IsEmpty after removing only peer")
	}
	// Validate must reject a peer without a key, or with garbage.
	bad := &FileBlock{Peers: []string{"https://x"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("peers w/o keys must fail Validate")
	}
	bad.PeerKeys = map[string]string{"https://x": "not-base64!!!"}
	if err := bad.Validate(); err == nil {
		t.Fatal("garbage base64 must fail Validate")
	}
	shortKey := base64.StdEncoding.EncodeToString([]byte("too-short"))
	bad.PeerKeys = map[string]string{"https://x": shortKey}
	if err := bad.Validate(); err == nil {
		t.Fatal("short-key must fail Validate")
	}
}

func TestPeerConfigsAndRoundtrip(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "weft.hcl")
	pubA := genB64Key(t)
	pubB := genB64Key(t)
	listen := ":9102"
	fb := &FileBlock{Listen: &listen}
	if err := fb.AddPeer("https://peer-a", pubA); err != nil {
		t.Fatal(err)
	}
	if err := fb.AddPeer("https://peer-b", pubB); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileBlock(cfg, fb); err != nil {
		t.Fatalf("Write : %v", err)
	}
	// The serialised form must mention each pubkey + listen ; cheap
	// sanity check that the cty value made it onto disk.
	body, _ := os.ReadFile(cfg)
	if !strings.Contains(string(body), ":9102") {
		t.Fatalf("listen missing from file :\n%s", body)
	}
	if !strings.Contains(string(body), pubA) {
		t.Fatalf("pubA missing from file :\n%s", body)
	}
	round, err := ReadFileBlock(cfg)
	if err != nil {
		t.Fatalf("Read : %v", err)
	}
	if round == nil {
		t.Fatal("round-tripped block must not be nil")
	}
	if round.Listen == nil || *round.Listen != ":9102" {
		t.Fatalf("listen : %+v", round.Listen)
	}
	configs, err := round.PeerConfigs()
	if err != nil {
		t.Fatalf("PeerConfigs : %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("len : %d", len(configs))
	}
	for _, pc := range configs {
		if len(pc.PublicKey) != ed25519.PublicKeySize {
			t.Fatalf("pubkey size : %d", len(pc.PublicKey))
		}
	}
	// Sanity : an absent file yields no peers, no error.
	configsAbsent, err := (&FileBlock{}).PeerConfigs()
	if err != nil || configsAbsent != nil {
		t.Fatalf("empty block : err=%v configs=%v", err, configsAbsent)
	}
}

func TestWritePreservesOtherBlocks(t *testing.T) {
	// hclwrite-based merge must keep unrelated top-level blocks
	// untouched. We don't fully parse HCL on read-back ; just
	// assert the bytes still contain the other block + its
	// comment after a federation rewrite.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "weft.hcl")
	pre := []byte(`# leading comment
oidc {
  issuer    = "https://dex.internal.example.com"
  client_id = "weft"
}
`)
	if err := os.WriteFile(cfg, pre, 0o600); err != nil {
		t.Fatalf("seed : %v", err)
	}
	pubA := genB64Key(t)
	fb := &FileBlock{}
	if err := fb.AddPeer("https://peer-a", pubA); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileBlock(cfg, fb); err != nil {
		t.Fatalf("Write : %v", err)
	}
	post, _ := os.ReadFile(cfg)
	s := string(post)
	if !strings.Contains(s, "dex.internal.example.com") {
		t.Fatalf("oidc block lost :\n%s", s)
	}
	if !strings.Contains(s, "leading comment") {
		t.Fatalf("leading comment lost :\n%s", s)
	}
	if !strings.Contains(s, "https://peer-a") {
		t.Fatalf("federation peer missing :\n%s", s)
	}
}
