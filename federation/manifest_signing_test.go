package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

// signedManifest returns a fresh manifest plus a freshly-generated
// ed25519 keypair. Centralised so the signing tests stay legible.
func signedManifest(t *testing.T) (*FederationManifest, ed25519.PublicKey, ed25519.PrivateKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 : %v", err)
	}
	m := vm()
	sig, err := m.Sign(priv)
	if err != nil {
		t.Fatalf("Sign : %v", err)
	}
	return m, pub, priv, sig
}

func TestSignVerifyRoundtrip(t *testing.T) {
	m, pub, _, sig := signedManifest(t)
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("sig length : got %d want %d", len(sig), ed25519.SignatureSize)
	}
	if err := m.Verify(pub, sig); err != nil {
		t.Fatalf("Verify : %v", err)
	}
	// Ed25519Verifier adapter must round-trip the same way.
	if err := (Ed25519Verifier{Pub: pub}).Verify(m, sig); err != nil {
		t.Fatalf("Ed25519Verifier : %v", err)
	}
}

func TestVerifyWrongKey(t *testing.T) {
	m, _, _, sig := signedManifest(t)
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen other : %v", err)
	}
	err = m.Verify(otherPub, sig)
	if err == nil {
		t.Fatal("Verify with wrong key must error")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("error should mention mismatch, got %v", err)
	}
}

func TestSignVerifyBadSizes(t *testing.T) {
	m := vm()
	if _, err := m.Sign(ed25519.PrivateKey{1, 2, 3}); err == nil {
		t.Fatal("Sign must reject short key")
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.Verify(ed25519.PublicKey{1, 2, 3}, make([]byte, ed25519.SignatureSize)); err == nil {
		t.Fatal("Verify must reject short pubkey")
	}
	if err := m.Verify(pub, []byte("short")); err == nil {
		t.Fatal("Verify must reject short signature")
	}
	// A structurally-invalid manifest must error before signature
	// math runs, even with the right key sizes.
	bad := &FederationManifest{Name: ""}
	if _, err := bad.Sign(make([]byte, ed25519.PrivateKeySize)); err == nil {
		t.Fatal("Sign of invalid manifest must error")
	}
	if err := bad.Verify(make([]byte, ed25519.PublicKeySize), make([]byte, ed25519.SignatureSize)); err == nil {
		t.Fatal("Verify of invalid manifest must error")
	}
}
