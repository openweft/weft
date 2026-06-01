package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPollOnceHappyPath(t *testing.T) {
	m, pub, priv := freshSigner(t)
	url, teardown := httpServerFor(t, m, priv)
	defer teardown()

	p := NewPoller([]PeerConfig{{URL: url, PublicKey: pub, Name: "peer-a"}}, time.Second, 5*time.Minute)
	// Drive the state map without starting the goroutine — Start is
	// covered by the stale-TTL test below.
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start : %v", err)
	}
	defer p.Stop()
	if err := p.PollOnce(context.Background(), p.Peers[0]); err != nil {
		t.Fatalf("PollOnce : %v", err)
	}
	snap := p.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snap len : %d", len(snap))
	}
	got := snap[0]
	if got.Status != "live" {
		t.Fatalf("status : %s", got.Status)
	}
	if got.LastSeen.IsZero() {
		t.Fatal("LastSeen must be populated on success")
	}
	if got.Manifest == nil || got.Manifest.Name != m.Name {
		t.Fatalf("manifest : %+v", got.Manifest)
	}
	if got.LastError != "" {
		t.Fatalf("LastError : %s", got.LastError)
	}
}

func TestPollOnceRejectsBadSignature(t *testing.T) {
	m, _, priv := freshSigner(t)
	// The poll uses a different public key than the server signed
	// with → verify must fail.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen : %v", err)
	}
	url, teardown := httpServerFor(t, m, priv)
	defer teardown()

	p := NewPoller([]PeerConfig{{URL: url, PublicKey: otherPub}}, time.Second, 5*time.Minute)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start : %v", err)
	}
	defer p.Stop()
	err = p.PollOnce(context.Background(), p.Peers[0])
	if err == nil {
		t.Fatal("PollOnce with wrong pubkey must error")
	}
	snap := p.Snapshot()
	if snap[0].Status != "unreachable" {
		t.Fatalf("status before any success : %s", snap[0].Status)
	}
	if snap[0].LastError == "" {
		t.Fatal("LastError must be recorded")
	}
	if snap[0].Manifest != nil {
		t.Fatal("Manifest must remain nil when no successful poll has landed")
	}
}

func TestPollOnceMissingSigHeader(t *testing.T) {
	m, pub, _ := freshSigner(t)
	// Hand-roll a server that returns JSON but no signature header.
	mux := http.NewServeMux()
	mux.HandleFunc(ClusterInfoPath, func(w http.ResponseWriter, _ *http.Request) {
		body, _ := m.Marshal()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := NewPoller([]PeerConfig{{URL: ts.URL, PublicKey: pub}}, time.Second, 5*time.Minute)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start : %v", err)
	}
	defer p.Stop()
	err := p.PollOnce(context.Background(), p.Peers[0])
	if err == nil {
		t.Fatal("missing signature header must error")
	}
}

func TestPollOnceMalformedSignatureHex(t *testing.T) {
	m, pub, _ := freshSigner(t)
	mux := http.NewServeMux()
	mux.HandleFunc(ClusterInfoPath, func(w http.ResponseWriter, _ *http.Request) {
		body, _ := m.Marshal()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(SignatureHeader, "not-hex-bytes")
		_, _ = w.Write(body)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	p := NewPoller([]PeerConfig{{URL: ts.URL, PublicKey: pub}}, time.Second, 5*time.Minute)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start : %v", err)
	}
	defer p.Stop()
	if err := p.PollOnce(context.Background(), p.Peers[0]); err == nil {
		t.Fatal("malformed hex sig must error")
	}
}

func TestStaleAfterTTL(t *testing.T) {
	m, pub, priv := freshSigner(t)
	url, teardown := httpServerFor(t, m, priv)
	defer teardown()
	// Controlled clock — start at T0, advance past TTL to confirm
	// the status flips from live → stale.
	clock := time.Unix(1_700_000_000, 0)
	p := NewPoller([]PeerConfig{{URL: url, PublicKey: pub}}, time.Second, 30*time.Second)
	p.Now = func() time.Time { return clock }
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start : %v", err)
	}
	defer p.Stop()
	if err := p.PollOnce(context.Background(), p.Peers[0]); err != nil {
		t.Fatalf("PollOnce : %v", err)
	}
	if got := p.Snapshot()[0].Status; got != "live" {
		t.Fatalf("just after poll : %s", got)
	}
	// Advance just past the TTL.
	clock = clock.Add(31 * time.Second)
	if got := p.Snapshot()[0].Status; got != "stale" {
		t.Fatalf("after TTL : %s", got)
	}
	// LiveManifests must filter the stale peer out.
	if live := p.LiveManifests(); len(live) != 0 {
		t.Fatalf("LiveManifests : got %d want 0", len(live))
	}
}

func TestPollerStartBadConfig(t *testing.T) {
	if err := (&Poller{}).Start(context.Background()); err == nil {
		t.Fatal("no peers must error")
	}
	if err := (&Poller{Peers: []PeerConfig{{URL: "", PublicKey: make([]byte, ed25519.PublicKeySize)}}}).Start(context.Background()); err == nil {
		t.Fatal("empty URL must error")
	}
	if err := (&Poller{Peers: []PeerConfig{{URL: "http://x", PublicKey: []byte("short")}}}).Start(context.Background()); err == nil {
		t.Fatal("short pubkey must error")
	}
	p := NewPoller([]PeerConfig{{URL: "http://x", PublicKey: make([]byte, ed25519.PublicKeySize)}}, time.Second, time.Second)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start : %v", err)
	}
	defer p.Stop()
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("double Start must error")
	}
}

func TestResolveClusterInfoURL(t *testing.T) {
	cases := map[string]string{
		"http://x":                   "http://x" + ClusterInfoPath,
		"http://x/":                  "http://x" + ClusterInfoPath,
		"http://x" + ClusterInfoPath: "http://x" + ClusterInfoPath,
	}
	for in, want := range cases {
		if got := resolveClusterInfoURL(in); got != want {
			t.Fatalf("resolve(%q) = %q want %q", in, got, want)
		}
	}
}

func TestPollOnceTamperedBodyRejected(t *testing.T) {
	// Server signs the canonical manifest then ships a body that's
	// been re-serialised with an extra field — Verify must reject
	// because the re-marshalled bytes won't match the original
	// signed bytes. We approximate this by signing manifest A but
	// returning manifest B.
	mA := vm()
	mB := vm()
	mB.Members[0].Region = "tampered"
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen : %v", err)
	}
	bA, _ := mA.Marshal()
	sig := ed25519.Sign(priv, bA)
	mux := http.NewServeMux()
	mux.HandleFunc(ClusterInfoPath, func(w http.ResponseWriter, _ *http.Request) {
		body, _ := mB.Marshal()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(SignatureHeader, hex.EncodeToString(sig))
		_, _ = w.Write(body)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	p := NewPoller([]PeerConfig{{URL: ts.URL, PublicKey: pub}}, time.Second, 5*time.Minute)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start : %v", err)
	}
	defer p.Stop()
	if err := p.PollOnce(context.Background(), p.Peers[0]); err == nil {
		t.Fatal("tampered body must fail signature verify")
	}
}
