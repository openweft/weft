package weft

// attestation.go is the control-plane attestation surface : the
// feature-flagged TPM remote-attestation gate that sits in FRONT of
// host admission (RegisterHost). It is PURE GO and never touches a TPM
// — the verifier side of the go-tpm2/attest protocol (MakeCredential,
// VerifyQuote, nonce checks) is all off-TPM crypto.
//
// Shape :
//
//   - AttestationGate bundles the etcd-backed EKRegistry, the
//     attest.Verifier, the short-TTL "recently admitted" set, and the
//     Enabled flag. One per control-plane process.
//   - When Enabled is FALSE (the default), RegisterHost behaves exactly
//     as it does today (OIDC RequireAdmin only) ; the gate's nil/disabled
//     state is the contract that keeps the existing path byte-for-byte
//     unchanged.
//   - When Enabled is TRUE, the four AttestationService RPCs drive the
//     verifier ; a successful Admit records the node's AK Name in the
//     admitted set with a TTL ; RegisterHost then requires the caller's
//     AK Name to be freshly admitted before s.adp.RegisterHost(spec).
//
// HA note : the admitted set + the verifier's pending enrolment/admission
// state are IN-MEMORY. A multi-replica control plane must move them to
// etcd (a follow-up) so an Admit served by dc2 is visible to a
// RegisterHost served by dc1. The EKRegistry itself is already etcd-backed.

import (
	"sync"
	"time"

	"github.com/go-tpm2/attest"
)

// DefaultAdmissionTTL is how long a successful Admit stays valid for a
// subsequent RegisterHost. Short by design : the node admits, then
// immediately registers. A generous-but-bounded window absorbs network
// latency + a slow registration without leaving a stale admit usable.
const DefaultAdmissionTTL = 5 * time.Minute

// AttestationGate is the control-plane attestation state. Construct it
// with NewAttestationGate ; a nil *AttestationGate means "attestation
// not wired", and IsEnabled() reports false (the RegisterHost OFF path).
type AttestationGate struct {
	// Enabled is the feature flag. False (the zero value) keeps the
	// RegisterHost path unchanged.
	Enabled bool

	// Registry is the etcd-backed EK trust store. nil when attestation
	// is not wired.
	Registry *EKRegistry

	// Verifier is the pure-Go go-tpm2/attest verifier. nil when not wired.
	Verifier *attest.Verifier

	// ttl is the admitted-set entry lifetime.
	ttl time.Duration
	// now is the clock, injectable for tests.
	now func() time.Time

	mu       sync.Mutex
	admitted map[string]time.Time // AK Name (string key) → admit deadline
}

// NewAttestationGate builds a gate over an EKRegistry + Verifier with
// the given enabled flag and TTL. A zero ttl uses DefaultAdmissionTTL.
func NewAttestationGate(enabled bool, reg *EKRegistry, v *attest.Verifier, ttl time.Duration) *AttestationGate {
	if ttl <= 0 {
		ttl = DefaultAdmissionTTL
	}
	return &AttestationGate{
		Enabled:  enabled,
		Registry: reg,
		Verifier: v,
		ttl:      ttl,
		now:      time.Now,
		admitted: make(map[string]time.Time),
	}
}

// IsEnabled reports whether the gate is wired AND turned on. A nil gate
// reports false — this is the predicate RegisterHost uses to decide
// between the unchanged OFF path and the attested ON path.
func (g *AttestationGate) IsEnabled() bool {
	return g != nil && g.Enabled && g.Verifier != nil
}

// markAdmitted records a successful admission for akName, valid for the
// gate's TTL. Called by the Admit RPC handler on a granted decision.
func (g *AttestationGate) MarkAdmitted(akName []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.admitted[string(akName)] = g.now().Add(g.ttl)
}

// IsAdmitted reports whether akName has a non-expired admission. Expired
// entries are swept on read. An empty akName is never admitted.
func (g *AttestationGate) IsAdmitted(akName []byte) bool {
	if len(akName) == 0 {
		return false
	}
	key := string(akName)
	g.mu.Lock()
	defer g.mu.Unlock()
	deadline, ok := g.admitted[key]
	if !ok {
		return false
	}
	if g.now().After(deadline) {
		delete(g.admitted, key)
		return false
	}
	return true
}

// consumeAdmission reports whether akName is admitted and, if so, drops
// the entry so a single admission grants exactly one registration
// (defence-in-depth against an admission being replayed across multiple
// RegisterHost calls). Returns false for an empty / unknown / expired AK.
func (g *AttestationGate) ConsumeAdmission(akName []byte) bool {
	if len(akName) == 0 {
		return false
	}
	key := string(akName)
	g.mu.Lock()
	defer g.mu.Unlock()
	deadline, ok := g.admitted[key]
	if !ok {
		return false
	}
	delete(g.admitted, key)
	return !g.now().After(deadline)
}
