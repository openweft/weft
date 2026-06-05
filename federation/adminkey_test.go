package federation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

// keyTestEnv assembles ed25519 + RSA keypairs for the table tests
// below. Kept as a helper so each test starts from a known set
// without re-paying the keygen cost on every assertion.
type keyTestEnv struct {
	edPub  ed25519.PublicKey
	edPriv ed25519.PrivateKey

	rsaPub  *rsa.PublicKey
	rsaPriv *rsa.PrivateKey

	// extras for rotation tests
	edPub2  ed25519.PublicKey
	edPriv2 ed25519.PrivateKey
}

func setupKeyEnv(t *testing.T) keyTestEnv {
	t.Helper()
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	edPub2, edPriv2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen (2): %v", err)
	}
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	return keyTestEnv{
		edPub: edPub, edPriv: edPriv,
		edPub2: edPub2, edPriv2: edPriv2,
		rsaPub: &rsaPriv.PublicKey, rsaPriv: rsaPriv,
	}
}

func pemBlock(t *testing.T, pub any, headers map[string]string) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Headers: headers, Bytes: der})
}

func sampleManifest() *FederationManifest {
	return &FederationManifest{
		Name:    "acme-global",
		Version: 7,
		Members: []Cluster{{Name: "eu-west", Region: "eu-west-3"}},
	}
}

func TestAdminKeyVerifier_DenyAllOnZero(t *testing.T) {
	if err := (AdminKeyVerifier{}).Verify(sampleManifest(), []byte("anything")); err == nil {
		t.Fatal("zero-value AdminKeyVerifier must deny ; got nil error")
	}
}

func TestAdminKeyVerifier_Ed25519RoundTrip(t *testing.T) {
	env := setupKeyEnv(t)
	m := sampleManifest()
	sig, err := m.Sign(env.edPriv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	v := AdminKeyVerifier{Keys: []AdminKey{
		{Algorithm: "ed25519", ed25519: env.edPub},
	}}
	if err := v.Verify(m, sig); err != nil {
		t.Fatalf("Verify ed25519 round trip: %v", err)
	}
}

func TestAdminKeyVerifier_RSARoundTrip(t *testing.T) {
	env := setupKeyEnv(t)
	m := sampleManifest()
	sig, err := m.SignRSA(env.rsaPriv)
	if err != nil {
		t.Fatalf("SignRSA: %v", err)
	}
	v := AdminKeyVerifier{Keys: []AdminKey{
		{Algorithm: "rsa", rsa: env.rsaPub},
	}}
	if err := v.Verify(m, sig); err != nil {
		t.Fatalf("Verify RSA round trip: %v", err)
	}
}

func TestAdminKeyVerifier_AcceptsAnyOfMultiple(t *testing.T) {
	// Rotation scenario : two ed25519 keys enrolled, manifest
	// signed by the second one. Order in Keys must not matter.
	env := setupKeyEnv(t)
	m := sampleManifest()
	sig, err := m.Sign(env.edPriv2)
	if err != nil {
		t.Fatalf("Sign (2): %v", err)
	}
	v := AdminKeyVerifier{Keys: []AdminKey{
		{Algorithm: "ed25519", ed25519: env.edPub},  // wrong key first
		{Algorithm: "ed25519", ed25519: env.edPub2}, // correct key second
	}}
	if err := v.Verify(m, sig); err != nil {
		t.Fatalf("multi-key Verify: %v", err)
	}
}

func TestAdminKeyVerifier_MixedAlgorithmKeyset(t *testing.T) {
	// Enrolment with one ed25519 + one RSA key ; verify works
	// against a signature from either side.
	env := setupKeyEnv(t)
	m := sampleManifest()
	v := AdminKeyVerifier{Keys: []AdminKey{
		{Algorithm: "ed25519", ed25519: env.edPub},
		{Algorithm: "rsa", rsa: env.rsaPub},
	}}
	edSig, _ := m.Sign(env.edPriv)
	rsaSig, _ := m.SignRSA(env.rsaPriv)
	if err := v.Verify(m, edSig); err != nil {
		t.Fatalf("ed25519 side: %v", err)
	}
	if err := v.Verify(m, rsaSig); err != nil {
		t.Fatalf("rsa side: %v", err)
	}
}

func TestAdminKeyVerifier_RejectsWrongSignature(t *testing.T) {
	env := setupKeyEnv(t)
	m := sampleManifest()
	// Real ed25519 sig over a *different* manifest. Same key class,
	// just won't match the current m bytes.
	other := &FederationManifest{Name: "different", Version: 1, Members: []Cluster{{Name: "x"}}}
	badSig, _ := other.Sign(env.edPriv)
	v := AdminKeyVerifier{Keys: []AdminKey{{Algorithm: "ed25519", ed25519: env.edPub}}}
	if err := v.Verify(m, badSig); err == nil {
		t.Fatal("verify must reject signature over different manifest body")
	}
	// Garbage of the right size — still rejected.
	garbage := bytes.Repeat([]byte{0x42}, ed25519.SignatureSize)
	if err := v.Verify(m, garbage); err == nil {
		t.Fatal("verify must reject garbage signature")
	}
}

func TestAdminKeyVerifier_RejectsWhenSigClassUnenrolled(t *testing.T) {
	env := setupKeyEnv(t)
	m := sampleManifest()
	rsaSig, _ := m.SignRSA(env.rsaPriv)
	// Only ed25519 keys enrolled — an RSA sig must NOT verify.
	v := AdminKeyVerifier{Keys: []AdminKey{{Algorithm: "ed25519", ed25519: env.edPub}}}
	if err := v.Verify(m, rsaSig); err == nil {
		t.Fatal("verify must reject RSA sig when no RSA key is enrolled")
	}
}

func TestLoadAdminKeysFromPEM_Mixed(t *testing.T) {
	env := setupKeyEnv(t)
	ed := pemBlock(t, env.edPub, map[string]string{"Comment": "alice@acme.org"})
	r := pemBlock(t, env.rsaPub, map[string]string{"Comment": "bob@acme.org"})
	bundle := append(append([]byte{}, ed...), r...)
	keys, err := LoadAdminKeysFromPEM(bundle)
	if err != nil {
		t.Fatalf("LoadAdminKeysFromPEM: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	if keys[0].Algorithm != "ed25519" || keys[0].Comment != "alice@acme.org" {
		t.Errorf("first key not the ed25519 one : %+v", keys[0])
	}
	if keys[1].Algorithm != "rsa" || keys[1].Comment != "bob@acme.org" {
		t.Errorf("second key not the RSA one : %+v", keys[1])
	}
	// End-to-end : enrol, sign, verify.
	v := AdminKeyVerifier{Keys: keys}
	m := sampleManifest()
	sig, _ := m.Sign(env.edPriv)
	if err := v.Verify(m, sig); err != nil {
		t.Fatalf("post-load verify: %v", err)
	}
}

func TestLoadAdminKeysFromPEM_EmptyOrJunk(t *testing.T) {
	if _, err := LoadAdminKeysFromPEM(nil); err == nil {
		t.Error("nil PEM input must error (deny-all is safer than silent empty)")
	}
	if _, err := LoadAdminKeysFromPEM([]byte("no PEM blocks here, just text")); err == nil {
		t.Error("non-PEM input must error")
	}
}

func TestLoadAdminKeysFromPEM_RejectsWeakRSA(t *testing.T) {
	// Generate a 1024-bit RSA key — must be rejected at enrolment.
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa keygen 1024: %v", err)
	}
	b := pemBlock(t, &weak.PublicKey, nil)
	if _, err := LoadAdminKeysFromPEM(b); err == nil {
		t.Fatal("LoadAdminKeysFromPEM must reject sub-2048-bit RSA")
	} else if !strings.Contains(err.Error(), "1024") {
		t.Errorf("error should mention the offending bit length, got: %v", err)
	}
}

func TestAdminKeyVerifier_PluggableInVerifyManifest(t *testing.T) {
	// The public helper must accept the new verifier just like
	// it accepts Ed25519Verifier/DenyAllVerifier — proves the
	// Verifier interface is honoured.
	env := setupKeyEnv(t)
	m := sampleManifest()
	sig, _ := m.Sign(env.edPriv)
	v := AdminKeyVerifier{Keys: []AdminKey{{Algorithm: "ed25519", ed25519: env.edPub}}}
	if err := VerifyManifest(v, m, sig); err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	// Wrong key → wrapped through VerifyManifest, still a clean
	// non-nil error.
	v2 := AdminKeyVerifier{Keys: []AdminKey{{Algorithm: "ed25519", ed25519: env.edPub2}}}
	if err := VerifyManifest(v2, m, sig); err == nil {
		t.Fatal("VerifyManifest with wrong key must fail")
	} else if errors.Is(err, errors.New("placeholder")) { // pin: type-erasure smell-test
		_ = err
	}
}

func TestAdminKeyVerifier_AppendKey(t *testing.T) {
	env := setupKeyEnv(t)
	v := AdminKeyVerifier{}
	v2 := v.AppendKey(AdminKey{Algorithm: "ed25519", ed25519: env.edPub})
	if len(v.Keys) != 0 {
		t.Errorf("AppendKey must not mutate the receiver ; got %d keys", len(v.Keys))
	}
	if len(v2.Keys) != 1 {
		t.Errorf("AppendKey result should have 1 key, got %d", len(v2.Keys))
	}
}
