package main

// attestation_test.go drives the four AttestationService RPC handlers
// end-to-end against the published go-tpm2/attest Verifier, then exercises
// the RegisterHost gate in both flag states.
//
// The attest package's own handshake helpers (newEK / newAK /
// attestBuilder / activateInverse) are package-internal _test.go code, so
// this file rebuilds the minimal node side from the PUBLIC tpm2 + common
// API : real P-256 EK + AK keypairs, the in-test inverse of
// ActivateCredential (to recover the activation secret without a TPM), and
// a hand-built signed TPMS_ATTEST quote. This mirrors attest's own
// verifier_test.go exactly, proving the round-trip without any TPM.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/go-tpm2/attest"
	"github.com/go-tpm2/common"
	"github.com/go-tpm2/tpm2"
	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---- node-side test helpers (rebuilt from the public tpm2/common API) ----

const (
	tAlgECC      = 0x0023
	tAlgSHA256   = 0x000B
	tAlgECDSA    = 0x0018
	tAlgAES      = 0x0006
	tAlgCFB      = 0x0043
	tEccNistP256 = 0x0003
	tAttestMagic = 0xFF544347
	tStQuote     = 0x8018
)

type testKey struct {
	priv *ecdsa.PrivateKey
	pub  []byte // TPMT_PUBLIC carrying the point
}

func fixed32be(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func newAKKey(t *testing.T) testKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return testKey{priv: priv, pub: akPublicArea(priv)}
}

func newEKKey(t *testing.T) testKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return testKey{priv: priv, pub: ekPublicArea(priv)}
}

func akPublicArea(priv *ecdsa.PrivateKey) []byte {
	var p []byte
	p = common.PutU16(p, tAlgECC)
	p = common.PutU16(p, tAlgSHA256)
	p = common.PutU32(p, 0x00050072)
	p = common.PutU16(p, 0)
	p = common.PutU16(p, 0x0010)
	p = common.PutU16(p, tAlgECDSA)
	p = common.PutU16(p, tAlgSHA256)
	p = common.PutU16(p, tEccNistP256)
	p = common.PutU16(p, 0x0010)
	p = append(p, common.MarshalTPM2B(fixed32be(priv.X))...)
	p = append(p, common.MarshalTPM2B(fixed32be(priv.Y))...)
	return p
}

func ekPublicArea(priv *ecdsa.PrivateKey) []byte {
	var p []byte
	p = common.PutU16(p, tAlgECC)
	p = common.PutU16(p, tAlgSHA256)
	p = common.PutU32(p, 0x000300B2)
	p = common.PutU16(p, 0)
	p = common.PutU16(p, tAlgAES)
	p = common.PutU16(p, 128)
	p = common.PutU16(p, tAlgCFB)
	p = common.PutU16(p, 0x0010)
	p = common.PutU16(p, tEccNistP256)
	p = common.PutU16(p, 0x0010)
	p = append(p, common.MarshalTPM2B(fixed32be(priv.X))...)
	p = append(p, common.MarshalTPM2B(fixed32be(priv.Y))...)
	return p
}

// activateInverse recovers MakeCredential's embedded secret using the EK
// private key + AK Name — the off-TPM stand-in for ActivateCredential.
func activateInverse(t *testing.T, ek *ecdsa.PrivateKey, akName, credentialBlob, secret []byte) []byte {
	t.Helper()
	inner, _, err := common.UnmarshalTPM2B(secret)
	if err != nil {
		t.Fatalf("secret outer TPM2B: %v", err)
	}
	qx, rest, err := common.UnmarshalTPM2B(inner)
	if err != nil {
		t.Fatalf("Qe.x: %v", err)
	}
	qy, _, err := common.UnmarshalTPM2B(rest)
	if err != nil {
		t.Fatalf("Qe.y: %v", err)
	}
	curve := elliptic.P256()
	qxI := new(big.Int).SetBytes(qx)
	qyI := new(big.Int).SetBytes(qy)
	zx, _ := curve.ScalarMult(qxI, qyI, ek.D.Bytes())
	z := leftPad32(zx.Bytes())

	ekX := fixed32be(ek.X)
	seed := tpm2.KDFe(z, "IDENTITY", leftPad32(qx), ekX, 256)
	symKey := tpm2.KDFa(seed, "STORAGE", akName, nil, 128)
	hmacKey := tpm2.KDFa(seed, "INTEGRITY", nil, nil, 256)

	idObj, _, err := common.UnmarshalTPM2B(credentialBlob)
	if err != nil {
		t.Fatalf("id object: %v", err)
	}
	outerHMAC, encIdentity, err := common.UnmarshalTPM2B(idObj)
	if err != nil {
		t.Fatalf("outer hmac: %v", err)
	}
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(encIdentity)
	mac.Write(akName)
	if !hmac.Equal(mac.Sum(nil), outerHMAC) {
		t.Fatalf("activateInverse: outer HMAC mismatch")
	}
	block, err := aes.NewCipher(symKey)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	iv := make([]byte, block.BlockSize())
	plain := make([]byte, len(encIdentity))
	cipher.NewCFBDecrypter(block, iv).XORKeyStream(plain, encIdentity)
	cred, _, err := common.UnmarshalTPM2B(plain)
	if err != nil {
		t.Fatalf("credential TPM2B: %v", err)
	}
	return append([]byte(nil), cred...)
}

func leftPad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// buildQuote hand-builds a signed TPMS_ATTEST quote over extraData + PCRs.
func buildQuote(t *testing.T, ak *ecdsa.PrivateKey, extraData []byte, pcrIdxs []int, pcrValues [][]byte) (quoted, sig []byte) {
	t.Helper()
	var a []byte
	a = common.PutU32(a, tAttestMagic)
	a = common.PutU16(a, tStQuote)
	a = append(a, common.MarshalTPM2B([]byte("signer"))...)
	a = append(a, common.MarshalTPM2B(extraData)...)
	a = common.PutU64(a, 1)
	a = common.PutU32(a, 0)
	a = common.PutU32(a, 0)
	a = common.PutU8(a, 1)
	a = common.PutU64(a, 0)
	a = append(a, marshalSelection(pcrIdxs)...)
	h := sha256.New()
	for _, v := range pcrValues {
		h.Write(v)
	}
	a = append(a, common.MarshalTPM2B(h.Sum(nil))...)

	digest := sha256.Sum256(a)
	r, s, err := ecdsa.Sign(rand.Reader, ak, digest[:])
	if err != nil {
		t.Fatalf("ecdsa.Sign: %v", err)
	}
	return a, attest.JoinSignature(tpm2.ECDSASignature{R: r.Bytes(), S: s.Bytes()})
}

func marshalSelection(pcrs []int) []byte {
	bitmap := make([]byte, 3)
	for _, p := range pcrs {
		if p >= 0 && p < 24 {
			bitmap[p/8] |= 1 << uint(p%8)
		}
	}
	out := common.PutU32(nil, 1)
	out = common.PutU16(out, tAlgSHA256)
	out = common.PutU8(out, 3)
	return append(out, bitmap...)
}

// ---- helpers to build an enabled / disabled weftServer ----

// newEnabledServer wires a weftServer with an enabled attestation gate
// over a fresh in-memory EK registry + a real verifier, returning the
// server and the gate's registry so the test can pre-trust the EK.
func newEnabledServer(t *testing.T) (*weftServer, *weft.EKRegistry) {
	t.Helper()
	kv := weft.NewMemKVStorage("/test/attest/")
	reg, err := weft.LoadEKRegistry(context.Background(), kv)
	if err != nil {
		t.Fatal(err)
	}
	v := attest.NewVerifier(reg, attest.GoldenPolicy{}, attest.RandNonce)
	gate := weft.NewAttestationGate(true, reg, v, 0)
	return &weftServer{attest: gate}, reg
}

// ---- the end-to-end handshake test ----

// TestAttestationHandshake_HappyPath drives Enroll → CompleteEnroll →
// RequestAdmission → Admit to a granted decision over the 4 RPC handlers,
// against a real verifier + a pre-trusted EK.
func TestAttestationHandshake_HappyPath(t *testing.T) {
	s, reg := newEnabledServer(t)
	ek := newEKKey(t)
	ak := newAKKey(t)
	akName, err := tpm2.ObjectName(ak.pub)
	if err != nil {
		t.Fatalf("ObjectName: %v", err)
	}
	if err := reg.TrustEK(marshalNodeEKPub(ek.priv)); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Enroll.
	er := attest.EnrollRequest{EKPub: marshalNodeEKPub(ek.priv), AKPub: ak.pub, AKName: akName}
	enrollResp, err := s.Enroll(ctx, &weftv1.AttestMsg{Payload: er.Marshal()})
	if err != nil {
		t.Fatalf("Enroll RPC: %v", err)
	}
	var ch attest.EnrollChallenge
	if err := ch.Unmarshal(enrollResp.GetPayload()); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	// 2. Node recovers the secret (off-TPM inverse) + CompleteEnroll.
	recovered := activateInverse(t, ek.priv, akName, ch.CredentialBlob, ch.Secret)
	proof := attest.EnrollProof{ActivationSecret: recovered}
	res, err := s.CompleteEnroll(ctx, &weftv1.AttestMsg{Payload: proof.Marshal(), AkName: akName})
	if err != nil {
		t.Fatalf("CompleteEnroll RPC: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("CompleteEnroll not ok : %s", res.GetReason())
	}

	// 3. RequestAdmission → nonce.
	ar := attest.AdmissionRequest{AKName: akName}
	chResp, err := s.RequestAdmission(ctx, &weftv1.AttestMsg{Payload: ar.Marshal(), AkName: akName})
	if err != nil {
		t.Fatalf("RequestAdmission RPC: %v", err)
	}
	var adCh attest.AdmissionChallenge
	if err := adCh.Unmarshal(chResp.GetPayload()); err != nil {
		t.Fatalf("decode admission challenge: %v", err)
	}

	// 4. Node quotes the nonce ; Admit grants.
	pcr0 := bytes.Repeat([]byte{0xa1}, 32)
	quoted, sig := buildQuote(t, ak.priv, adCh.Nonce[:], []int{0}, [][]byte{pcr0})
	resp := attest.AdmissionResponse{Quoted: quoted, Signature: sig, PCRs: map[int][]byte{0: pcr0}}
	admit, err := s.Admit(ctx, &weftv1.AttestMsg{Payload: resp.Marshal(), AkName: akName})
	if err != nil {
		t.Fatalf("Admit RPC: %v", err)
	}
	if !admit.GetGranted() {
		t.Fatalf("Admit not granted : %s", admit.GetReason())
	}

	// The AK is now in the admitted set : the gate would pass RegisterHost.
	if !s.attest.IsAdmitted(akName) {
		t.Fatal("AK not in admitted set after granted Admit")
	}
}

// marshalNodeEKPub renders the EK point in the same TPMT_PUBLIC shape the
// node ships (so TrustEK and EnrollRequest agree on the bytes).
func marshalNodeEKPub(priv *ecdsa.PrivateKey) []byte {
	return ekPublicArea(priv)
}

// TestAttestation_EnrollUntrustedEK : an EK not pre-trusted is refused
// with PermissionDenied.
func TestAttestation_EnrollUntrustedEK(t *testing.T) {
	s, _ := newEnabledServer(t)
	ek := newEKKey(t)
	ak := newAKKey(t)
	akName, _ := tpm2.ObjectName(ak.pub)
	er := attest.EnrollRequest{EKPub: ekPublicArea(ek.priv), AKPub: ak.pub, AKName: akName}
	_, err := s.Enroll(context.Background(), &weftv1.AttestMsg{Payload: er.Marshal()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v ; want PermissionDenied", err)
	}
}

// TestAttestation_DisabledRPCsUnavailable : with the gate off, every RPC
// returns Unavailable (the service is registered but inert).
func TestAttestation_DisabledRPCsUnavailable(t *testing.T) {
	s := &weftServer{attest: weft.NewAttestationGate(false, nil, nil, 0)}
	ctx := context.Background()
	if _, err := s.Enroll(ctx, &weftv1.AttestMsg{}); status.Code(err) != codes.Unavailable {
		t.Errorf("Enroll: got %v want Unavailable", err)
	}
	if _, err := s.CompleteEnroll(ctx, &weftv1.AttestMsg{AkName: []byte("x")}); status.Code(err) != codes.Unavailable {
		t.Errorf("CompleteEnroll: got %v want Unavailable", err)
	}
	if _, err := s.RequestAdmission(ctx, &weftv1.AttestMsg{}); status.Code(err) != codes.Unavailable {
		t.Errorf("RequestAdmission: got %v want Unavailable", err)
	}
	if _, err := s.Admit(ctx, &weftv1.AttestMsg{AkName: []byte("x")}); status.Code(err) != codes.Unavailable {
		t.Errorf("Admit: got %v want Unavailable", err)
	}
}

// TestAttestation_AdmitDeniedInBand : a quote that fails verification
// returns granted=false + reason (NOT a transport error).
func TestAttestation_AdmitDeniedInBand(t *testing.T) {
	s, reg := newEnabledServer(t)
	ek := newEKKey(t)
	ak := newAKKey(t)
	akName, _ := tpm2.ObjectName(ak.pub)
	_ = reg.TrustEK(ekPublicArea(ek.priv))

	ctx := context.Background()
	er := attest.EnrollRequest{EKPub: ekPublicArea(ek.priv), AKPub: ak.pub, AKName: akName}
	enrollResp, _ := s.Enroll(ctx, &weftv1.AttestMsg{Payload: er.Marshal()})
	var ch attest.EnrollChallenge
	_ = ch.Unmarshal(enrollResp.GetPayload())
	recovered := activateInverse(t, ek.priv, akName, ch.CredentialBlob, ch.Secret)
	_, _ = s.CompleteEnroll(ctx, &weftv1.AttestMsg{Payload: (attest.EnrollProof{ActivationSecret: recovered}).Marshal(), AkName: akName})
	_, _ = s.RequestAdmission(ctx, &weftv1.AttestMsg{Payload: (attest.AdmissionRequest{AKName: akName}).Marshal(), AkName: akName})

	// Sign with the WRONG key → ErrQuoteSignature → granted=false.
	other := newAKKey(t)
	pcr := bytes.Repeat([]byte{0x33}, 32)
	// reuse a fresh challenge nonce : fetch a new one.
	chResp, _ := s.RequestAdmission(ctx, &weftv1.AttestMsg{Payload: (attest.AdmissionRequest{AKName: akName}).Marshal(), AkName: akName})
	var adCh attest.AdmissionChallenge
	_ = adCh.Unmarshal(chResp.GetPayload())
	quoted, sig := buildQuote(t, other.priv, adCh.Nonce[:], []int{0}, [][]byte{pcr})
	resp := attest.AdmissionResponse{Quoted: quoted, Signature: sig, PCRs: map[int][]byte{0: pcr}}
	admit, err := s.Admit(ctx, &weftv1.AttestMsg{Payload: resp.Marshal(), AkName: akName})
	if err != nil {
		t.Fatalf("Admit RPC returned transport error : %v", err)
	}
	if admit.GetGranted() {
		t.Fatal("Admit granted a wrong-key quote")
	}
	if admit.GetReason() == "" {
		t.Fatal("Admit deny had no reason")
	}
}

// TestAttestation_MissingAKName : CompleteEnroll / Admit without ak_name
// are InvalidArgument.
func TestAttestation_MissingAKName(t *testing.T) {
	s, _ := newEnabledServer(t)
	ctx := context.Background()
	if _, err := s.CompleteEnroll(ctx, &weftv1.AttestMsg{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CompleteEnroll no ak_name: got %v want InvalidArgument", err)
	}
	if _, err := s.Admit(ctx, &weftv1.AttestMsg{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Admit no ak_name: got %v want InvalidArgument", err)
	}
}

// ---- the RegisterHost gate tests ----

// TestRegisterHostGate_FlagOff_Unchanged : with no attestation gate (the
// default), RegisterHost succeeds through the legacy path WITHOUT any
// attestation, and stamps no AK Name.
func TestRegisterHostGate_FlagOff_Unchanged(t *testing.T) {
	a := weft.NewWithKVStorage(t.TempDir(), nil, nil)
	s := &weftServer{adp: a} // attest == nil → OFF path
	resp, err := s.RegisterHost(devCtx(), &weftv1.RegisterHostRequest{
		Hostname: "compute-off", Hypervisor: "qemu", Architecture: "arm64",
	})
	if err != nil {
		t.Fatalf("RegisterHost (flag off) failed: %v", err)
	}
	if resp.GetHost().GetHostname() != "compute-off" {
		t.Fatalf("unexpected host: %+v", resp.GetHost())
	}
}

// TestRegisterHostGate_FlagOn_RejectsUnadmitted : with the gate enabled,
// a RegisterHost carrying no admitted AK is denied.
func TestRegisterHostGate_FlagOn_RejectsUnadmitted(t *testing.T) {
	s, _ := newEnabledServer(t)
	s.adp = weft.NewWithKVStorage(t.TempDir(), nil, nil)

	// No AK label at all.
	_, err := s.RegisterHost(devCtx(), &weftv1.RegisterHostRequest{
		Hostname: "compute-on", Hypervisor: "qemu", Architecture: "arm64",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v ; want PermissionDenied for missing ak", err)
	}

	// An AK label that was never admitted.
	_, err = s.RegisterHost(devCtx(), &weftv1.RegisterHostRequest{
		Hostname: "compute-on", Hypervisor: "qemu", Architecture: "arm64",
		Properties: map[string]string{attestAKLabel: "never-admitted-ak"},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v ; want PermissionDenied for un-admitted ak", err)
	}
}

// TestRegisterHostGate_FlagOn_AllowsAdmitted : with the gate enabled and
// the AK freshly admitted, RegisterHost succeeds and stamps the AK Name
// onto the Host entry — and the admission is single-use (a second
// RegisterHost with the same AK is denied).
func TestRegisterHostGate_FlagOn_AllowsAdmitted(t *testing.T) {
	s, _ := newEnabledServer(t)
	s.adp = weft.NewWithKVStorage(t.TempDir(), nil, nil)

	akName := []byte("admitted-ak-name")
	s.attest.MarkAdmitted(akName)

	if _, err := s.RegisterHost(devCtx(), &weftv1.RegisterHostRequest{
		Hostname: "compute-admitted", Hypervisor: "qemu", Architecture: "arm64",
		Properties: map[string]string{attestAKLabel: string(akName)},
	}); err != nil {
		t.Fatalf("RegisterHost (admitted) failed: %v", err)
	}
	// The Host entry carries the AK Name : verify via the adapter.
	h, ok := s.adp.HostByHostname("compute-admitted")
	if !ok {
		t.Fatal("host not registered")
	}
	if h.AKName != string(akName) {
		t.Fatalf("AKName not stamped: %q", h.AKName)
	}

	// Single-use : a second register with the same (now-consumed) AK fails.
	_, err := s.RegisterHost(devCtx(), &weftv1.RegisterHostRequest{
		Hostname: "compute-admitted-2", Hypervisor: "qemu", Architecture: "arm64",
		Properties: map[string]string{attestAKLabel: string(akName)},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("second register with consumed AK: got %v want PermissionDenied", err)
	}
}
