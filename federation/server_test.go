package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// freshSigner returns a manifest + ed25519 keypair, useful across
// the server / poller test files.
func freshSigner(t *testing.T) (*FederationManifest, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ed25519 : %v", err)
	}
	return vm(), pub, priv
}

// httpServerFor spins up an httptest server backed by federation.Server.
// Returns the server URL and a teardown.
func httpServerFor(t *testing.T, m *FederationManifest, priv ed25519.PrivateKey) (string, func()) {
	t.Helper()
	srv := &Server{Provider: StaticManifest{M: m}, PrivateKey: priv}
	ts := httptest.NewServer(srv.Handler())
	return ts.URL, ts.Close
}

func TestServerServesJSONAndSignatureHeader(t *testing.T) {
	m, pub, priv := freshSigner(t)
	url, teardown := httpServerFor(t, m, priv)
	defer teardown()

	resp, err := http.Get(url + ClusterInfoPath)
	if err != nil {
		t.Fatalf("GET : %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status : got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type : got %q want application/json", ct)
	}
	sigHex := resp.Header.Get(SignatureHeader)
	if sigHex == "" {
		t.Fatal("missing X-Cluster-Signature header")
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		t.Fatalf("decode sig : %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body : %v", err)
	}
	// Body must decode to a FederationManifest matching the served one.
	var got FederationManifest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body : %v", err)
	}
	if got.Name != m.Name || len(got.Members) != len(m.Members) {
		t.Fatalf("decoded mismatch : %+v", got)
	}
	// Signature must verify against the served body (using the
	// manifest's own Verify, which re-marshals — guards against
	// whitespace drift between server-rendered + client-recomputed
	// JSON).
	if err := got.Verify(pub, sig); err != nil {
		t.Fatalf("verify body sig : %v", err)
	}
}

func TestServerRejectsNonGET(t *testing.T) {
	_, _, priv := freshSigner(t)
	srv := &Server{Provider: StaticManifest{M: vm()}, PrivateKey: priv}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+ClusterInfoPath, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST : %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status : got %d want 405", resp.StatusCode)
	}
}

func TestServerMissingProvider(t *testing.T) {
	_, _, priv := freshSigner(t)
	srv := &Server{PrivateKey: priv}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + ClusterInfoPath)
	if err != nil {
		t.Fatalf("GET : %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status : got %d want 500", resp.StatusCode)
	}
}

func TestServerStartStopBindsPort(t *testing.T) {
	_, _, priv := freshSigner(t)
	srv := &Server{Provider: StaticManifest{M: vm()}, PrivateKey: priv}
	if err := srv.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Start : %v", err)
	}
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("Addr empty after Start")
	}
	// Round-trip against the real bind.
	resp, err := http.Get("http://" + addr + ClusterInfoPath)
	if err != nil {
		t.Fatalf("GET : %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status : got %d want 200", resp.StatusCode)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop : %v", err)
	}
	// Second Stop must be a no-op (idempotency).
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop second : %v", err)
	}
	if err := srv.Start(""); err == nil {
		t.Fatal("Start with empty addr must error")
	}
}
