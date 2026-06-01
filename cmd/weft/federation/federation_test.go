package federation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkgfed "github.com/openweft/weft/federation"
)

func freshPubB64(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen : %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func TestRunJoinAndLeave(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "weft.hcl")
	pub := freshPubB64(t)
	if err := RunJoin(cfg, "https://peer-a", pub); err != nil {
		t.Fatalf("RunJoin : %v", err)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("config file not created : %v", err)
	}
	// Idempotency : second join keeps the file byte-identical.
	before, _ := os.ReadFile(cfg)
	if err := RunJoin(cfg, "https://peer-a", pub); err != nil {
		t.Fatalf("RunJoin (idem) : %v", err)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Fatalf("re-join must be byte-identical\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	if err := RunLeave(cfg, "https://peer-a"); err != nil {
		t.Fatalf("RunLeave : %v", err)
	}
	// Leave of a missing peer must error.
	if err := RunLeave(cfg, "https://peer-a"); err == nil {
		t.Fatal("RunLeave twice must error")
	}
}

func TestRunListJSONAndTable(t *testing.T) {
	peers := []pkgfed.PeerState{
		{Name: "eu-paris", URL: "https://eu-paris", Status: "live", LastSeen: time.Unix(1_700_000_000, 0)},
		{Name: "us-iad", URL: "https://us-iad", Status: "unreachable"},
	}
	var buf bytes.Buffer
	if err := RunList(&buf, peers, "json"); err != nil {
		t.Fatalf("RunList json : %v", err)
	}
	var decoded []pkgfed.PeerState
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode : %v", err)
	}
	if len(decoded) != 2 || decoded[0].Name != "eu-paris" {
		t.Fatalf("decoded : %+v", decoded)
	}
	// Table format must mention both names + the header.
	buf.Reset()
	if err := RunList(&buf, peers, ""); err != nil {
		t.Fatalf("RunList table : %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "STATUS", "eu-paris", "us-iad", "live", "unreachable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q :\n%s", want, out)
		}
	}
}

func TestRunPlace(t *testing.T) {
	peers := []pkgfed.PeerState{
		{Name: "eu-fra", URL: "https://eu-fra", Status: "live", LastSeen: time.Now(),
			Manifest: &pkgfed.FederationManifest{
				Name: "acme", Version: 1,
				Members: []pkgfed.Cluster{{Name: "eu-fra", Region: "eu-west-3", Weight: 80}},
			},
		},
		{Name: "us-iad", URL: "https://us-iad", Status: "live", LastSeen: time.Now(),
			Manifest: &pkgfed.FederationManifest{
				Name: "acme", Version: 1,
				Members: []pkgfed.Cluster{{Name: "us-iad", Region: "us-east-1", Weight: 50}},
			},
		},
	}
	var buf bytes.Buffer
	if err := RunPlace(&buf, peers, pkgfed.Constraints{Region: "eu-west-3"}, ""); err != nil {
		t.Fatalf("RunPlace : %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "eu-fra") {
		t.Fatalf("eu-fra missing from output :\n%s", out)
	}
	// No-candidate path : empty peers + filter that excludes
	// everything must error so scripts notice the empty result.
	if err := RunPlace(&bytes.Buffer{}, nil, pkgfed.Constraints{}, ""); err == nil {
		t.Fatal("empty candidates must error")
	}
}

func TestFileSnapshotSource(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "weft.hcl")
	if err := RunJoin(cfg, "https://peer-x", freshPubB64(t)); err != nil {
		t.Fatalf("seed : %v", err)
	}
	src := FileSnapshotSource{Path: cfg}
	snap := src.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snap : %d", len(snap))
	}
	if snap[0].URL != "https://peer-x" || snap[0].Status != "unconfigured" {
		t.Fatalf("row : %+v", snap[0])
	}
	// Missing file → empty snap, no panic.
	if got := (FileSnapshotSource{Path: filepath.Join(dir, "missing.hcl")}).Snapshot(); got != nil {
		t.Fatalf("missing file must yield nil snap, got %v", got)
	}
}

func TestCommandWiring(t *testing.T) {
	// The cobra tree must at least construct without panicking, with
	// the expected verbs registered.
	cmd := Command()
	if cmd.Use != "federation" {
		t.Fatalf("Use : %q", cmd.Use)
	}
	verbs := map[string]bool{}
	for _, sub := range cmd.Commands() {
		// cobra Use strings include arg shape ; first word is verb.
		verbs[strings.SplitN(sub.Use, " ", 2)[0]] = true
	}
	for _, v := range []string{"join", "leave", "list", "place"} {
		if !verbs[v] {
			t.Fatalf("verb %q missing : %v", v, verbs)
		}
	}
}

func TestWithSourceSeam(t *testing.T) {
	// The WithSource seam must restore the previous source on its
	// returned func — so test isolation actually holds.
	called := false
	restore := WithSource(func(string) SnapshotSource {
		called = true
		return FileSnapshotSource{}
	})
	if listSource("/x") == nil {
		t.Fatal("listSource returned nil")
	}
	if !called {
		t.Fatal("override not used")
	}
	restore()
	// After restore, the factory must be the default again.
	_ = listSource("/x")
}
