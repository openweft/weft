// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

import (
	"crypto/rand"
	"crypto/subtle"
	"io"
	"sync"

	"github.com/go-tpm2/common"
	"github.com/go-tpm2/tpm2"
)

// Nonce is the verifier's source of fresh 32-byte challenges. It is injected so
// tests can supply a deterministic source; production code uses RandNonce.
type Nonce func() ([32]byte, error)

// RandNonce is the default Nonce source, drawing from crypto/rand.
func RandNonce() ([32]byte, error) {
	return nonceFrom(rand.Reader)
}

// nonceFrom reads a 32-byte nonce from r (factored out so the read-failure
// branch is testable with a failing reader).
func nonceFrom(r io.Reader) ([32]byte, error) {
	var n [32]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return [32]byte{}, err
	}
	return n, nil
}

// Verifier error sentinels (typed and constant for ==).
const (
	// ErrUnknownEK is returned by Enroll when the presented EK is not trusted.
	ErrUnknownEK = common.Error("attest: unknown or untrusted EK")
	// ErrActivationFailed is returned by CompleteEnroll when the returned
	// activation secret does not match the issued one (or no enrolment is
	// pending for the AK).
	ErrActivationFailed = common.Error("attest: credential activation failed")
	// ErrUnboundAK is returned when an admission step references an AK that was
	// never bound via a successful enrolment.
	ErrUnboundAK = common.Error("attest: AK is not bound")
	// ErrStaleNonce is returned by Admit when no challenge is pending for the
	// AK or the quote's extraData does not equal the issued nonce (anti-replay).
	ErrStaleNonce = common.Error("attest: stale or replayed nonce")
	// ErrQuoteSignature is returned when the quote's ECDSA signature does not
	// verify under the bound AK public key.
	ErrQuoteSignature = common.Error("attest: quote signature invalid")
	// ErrPCRDigestMismatch is returned when the quoted pcrDigest does not equal
	// the digest recomputed over the claimed PCR values.
	ErrPCRDigestMismatch = common.Error("attest: quoted pcrDigest != claimed PCRs")
	// ErrMalformedQuote is returned when the quote or signature cannot be
	// parsed (e.g. a signature that is not the expected r||s width).
	ErrMalformedQuote = common.Error("attest: malformed quote or signature")
)

// nonceLen is the fixed challenge/extraData width (matches AdmissionChallenge).
const nonceLen = 32

// p256HalfLen is the byte width of one half (r or s) of a P-256 ECDSA signature
// carried as the concatenated r||s in AdmissionResponse.Signature.
const p256HalfLen = 32

// Verifier is the control-plane side of the protocol: it admits nodes onto a
// fleet on proof of an approved boot. It is PURE GO and never touches a TPM —
// the verifier side of TPM credential protection (MakeCredential) and of quote
// verification (VerifyQuote) is all off-TPM crypto. A Verifier is safe for
// concurrent use.
type Verifier struct {
	registry EKRegistry
	policy   Policy
	nonce    Nonce
	// rng is the source for the random activation secret in Enroll. It
	// defaults to crypto/rand; tests override it to exercise the read-failure
	// branch.
	rng io.Reader

	mu             sync.Mutex
	pendingEnroll  map[string][]byte   // AKName -> issued activation secret
	pendingEnrollP map[string][]byte   // AKName -> AK public (to BindAK on success)
	pendingNonce   map[string][32]byte // AKName -> issued admission nonce
}

// NewVerifier builds a Verifier over a trust registry, an admission policy, and
// a nonce source. Pass RandNonce for production; a deterministic source for
// tests.
func NewVerifier(registry EKRegistry, policy Policy, nonce Nonce) *Verifier {
	return &Verifier{
		registry:       registry,
		policy:         policy,
		nonce:          nonce,
		rng:            rand.Reader,
		pendingEnroll:  make(map[string][]byte),
		pendingEnrollP: make(map[string][]byte),
		pendingNonce:   make(map[string][32]byte),
	}
}

// SetPolicy swaps the admission Policy. It is used after the first boot to
// install a GoldenPolicy built from the freshly measured PCRs (the common
// "trust on first attestation" bootstrap). It is safe for concurrent use.
func (v *Verifier) SetPolicy(p Policy) {
	v.mu.Lock()
	v.policy = p
	v.mu.Unlock()
}

// Enroll begins enrolment: it rejects an untrusted EK, draws a random
// activation secret, and runs MakeCredential OFF the TPM (from the EK public
// point and the AK Name) to produce the credentialBlob/secret challenge. The
// activation secret is stashed pending the node's proof. Only a TPM holding the
// EK private key and an AK with this exact Name can recover the secret.
func (v *Verifier) Enroll(req EnrollRequest) (EnrollChallenge, error) {
	if !v.registry.Trusted(req.EKPub) {
		return EnrollChallenge{}, ErrUnknownEK
	}
	// The EK point is parsed from the EK's TPMT_PUBLIC. A malformed EK public
	// is an untrusted EK as far as enrolment is concerned.
	ekPoint, err := parseECCPoint(req.EKPub)
	if err != nil {
		return EnrollChallenge{}, ErrUnknownEK
	}

	// Random 32-byte activation secret (a TPM2B_DIGEST-sized payload).
	secret := make([]byte, nonceLen)
	if _, err := io.ReadFull(v.rng, secret); err != nil {
		return EnrollChallenge{}, err
	}

	mc, err := tpm2.MakeCredential(tpm2.EKPublic{X: ekPoint.X, Y: ekPoint.Y}, req.AKName, secret, nil)
	if err != nil {
		return EnrollChallenge{}, err
	}

	v.mu.Lock()
	v.pendingEnroll[string(req.AKName)] = secret
	cp := make([]byte, len(req.AKPub))
	copy(cp, req.AKPub)
	v.pendingEnrollP[string(req.AKName)] = cp
	v.mu.Unlock()

	return EnrollChallenge{CredentialBlob: mc.CredentialBlob, Secret: mc.Secret}, nil
}

// CompleteEnroll finishes enrolment: it constant-time-compares the node's
// recovered ActivationSecret to the pending one and, on a match, binds the AK
// to the trusted EK in the registry. The pending entry is dropped either way
// (so a failed proof cannot be retried against the same challenge).
func (v *Verifier) CompleteEnroll(akName []byte, proof EnrollProof) error {
	key := string(akName)
	v.mu.Lock()
	want, ok := v.pendingEnroll[key]
	akPub := v.pendingEnrollP[key]
	delete(v.pendingEnroll, key)
	delete(v.pendingEnrollP, key)
	v.mu.Unlock()

	if !ok {
		return ErrActivationFailed
	}
	if subtle.ConstantTimeCompare(want, proof.ActivationSecret) != 1 {
		return ErrActivationFailed
	}
	return v.registry.BindAK(akName, akPub)
}

// Challenge issues a fresh admission nonce for a bound AK and stashes it
// pending the node's quote. An unbound AK is refused.
func (v *Verifier) Challenge(req AdmissionRequest) (AdmissionChallenge, error) {
	if _, ok := v.registry.AKPub(req.AKName); !ok {
		return AdmissionChallenge{}, ErrUnboundAK
	}
	n, err := v.nonce()
	if err != nil {
		return AdmissionChallenge{}, err
	}
	v.mu.Lock()
	v.pendingNonce[string(req.AKName)] = n
	v.mu.Unlock()
	return AdmissionChallenge{Nonce: n}, nil
}

// Admit decides whether to grant the node admission. It looks up the bound AK
// and the pending nonce, then in order:
//
//  1. verifies the Quote's ECDSA signature under the AK public AND that the
//     quoted pcrDigest equals the digest over the claimed PCRs (tpm2.VerifyQuote
//     does both);
//  2. checks the attest's extraData equals exactly the issued nonce
//     (anti-replay — VerifyQuote returns the parsed AttestInfo but does NOT
//     check the nonce, so it is checked explicitly here);
//  3. applies the boot Policy to the claimed PCRs, passing the attested event
//     log too (an EventLogPolicy replays it and binds it to these PCRs; a
//     GoldenPolicy ignores it).
//
// Each failure returns a precise sentinel. The pending nonce is consumed on
// every call so a quote cannot be replayed against the same challenge.
func (v *Verifier) Admit(akName []byte, resp AdmissionResponse) (bool, error) {
	key := string(akName)

	akPub, ok := v.registry.AKPub(akName)
	if !ok {
		return false, ErrUnboundAK
	}

	v.mu.Lock()
	nonce, haveNonce := v.pendingNonce[key]
	delete(v.pendingNonce, key)
	v.mu.Unlock()
	if !haveNonce {
		return false, ErrStaleNonce
	}

	point, err := parseECCPoint(akPub)
	if err != nil {
		return false, ErrMalformedQuote
	}
	sig, err := splitSignature(resp.Signature)
	if err != nil {
		return false, err
	}

	// VerifyQuote needs the selected PCR values in ascending selection order,
	// exactly as PCRRead returns them. Build that ordered slice from the map.
	expected := orderedPCRs(resp.PCRs)

	info, err := tpm2.VerifyQuote(tpm2.AKPublic{X: point.X, Y: point.Y}, resp.Quoted, sig, expected)
	if err != nil {
		switch err {
		case tpm2.ErrSigInvalid:
			return false, ErrQuoteSignature
		case tpm2.ErrPCRDigestMismatch:
			return false, ErrPCRDigestMismatch
		default:
			// Bad magic / not-a-quote / short buffer: a malformed quote.
			return false, ErrMalformedQuote
		}
	}

	// Anti-replay: the quote must be over THIS challenge's nonce. VerifyQuote
	// returns extraData but does not compare it, so it is checked here.
	if subtle.ConstantTimeCompare(info.ExtraData, nonce[:]) != 1 {
		return false, ErrStaleNonce
	}

	// The event log (if any) is fed to the Policy alongside the verified PCRs.
	// An EventLogPolicy replays it and binds it to these PCRs; value-only
	// policies (GoldenPolicy) ignore it.
	if err := v.policy.Matches(resp.PCRs, resp.EventLog); err != nil {
		return false, err
	}
	return true, nil
}

// orderedPCRs returns the PCR values ordered by ascending index, matching the
// TPM's selection order (the order PCRRead and the pcrDigest use).
func orderedPCRs(pcrs map[int][]byte) [][]byte {
	idxs := make([]int, 0, len(pcrs))
	for i := range pcrs {
		idxs = append(idxs, i)
	}
	sortInts(idxs)
	out := make([][]byte, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, pcrs[i])
	}
	return out
}

// splitSignature converts the concatenated r||s P-256 signature carried on the
// wire into a tpm2.ECDSASignature (the form VerifyQuote consumes). The two
// halves are each p256HalfLen bytes.
func splitSignature(sig []byte) (tpm2.ECDSASignature, error) {
	if len(sig) != 2*p256HalfLen {
		return tpm2.ECDSASignature{}, ErrMalformedQuote
	}
	r := make([]byte, p256HalfLen)
	s := make([]byte, p256HalfLen)
	copy(r, sig[:p256HalfLen])
	copy(s, sig[p256HalfLen:])
	return tpm2.ECDSASignature{R: r, S: s}, nil
}

// JoinSignature is the inverse of splitSignature: it renders a tpm2.ECDSASignature
// (as Quote returns it) into the fixed-width r||s wire form AdmissionResponse
// carries. r and s are left-padded to p256HalfLen bytes.
func JoinSignature(sig tpm2.ECDSASignature) []byte {
	out := make([]byte, 2*p256HalfLen)
	copyRightAligned(out[:p256HalfLen], sig.R)
	copyRightAligned(out[p256HalfLen:], sig.S)
	return out
}

// copyRightAligned copies src into the low-order (right) end of dst, leaving
// any leading bytes of dst zero (big-endian left-pad). If src is longer than
// dst, its low-order len(dst) bytes are kept.
func copyRightAligned(dst, src []byte) {
	if len(src) >= len(dst) {
		copy(dst, src[len(src)-len(dst):])
		return
	}
	copy(dst[len(dst)-len(src):], src)
}

// sortInts sorts a slice of ints ascending (a tiny insertion sort to avoid a
// sort import here; the slices are PCR-count small).
func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
