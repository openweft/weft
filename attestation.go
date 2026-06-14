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
// HA note : for a multi-replica control plane BOTH the admitted set and
// the verifier's single-use challenge state must be shared so an Admit
// served by dc2 is visible to a RegisterHost served by dc1. This is now
// wired :
//
//   - the verifier's pending enrolment-secret / admission-nonce state is
//     backed by an EtcdPendingStore (pendingstore_kv.go) when the gate is
//     built with a KVStorage (NewVerifierWithStore in the gate startup) ;
//   - the admitted set is backed by the same KVStorage here (a short-TTL
//     key per admitted AK, written by MarkAdmitted, consumed by
//     ConsumeAdmission = get-then-delete = single-use), with the in-memory
//     map as the fallback when no KVStorage is wired (dev / single-process).
//
// The EKRegistry itself is already etcd-backed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	// kv, when non-nil, backs the admitted set with a shared KVStorage so an
	// Admit on one replica is visible to a RegisterHost on another (HA). When
	// nil, the in-memory admitted map below is used (dev / single-process).
	kv KVStorage

	mu       sync.Mutex
	admitted map[string]time.Time // AK Name (string key) → admit deadline
}

// admittedNS is the per-record key namespace for the KV-backed admitted set,
// under the gate's shared KVStorage prefix.
const admittedNS = "admitted/"

// admittedRecord is the stored form of one admitted AK : its absolute expiry
// deadline (Unix nanoseconds). The AK Name itself is the hashed key.
type admittedRecord struct {
	ExpiresAt int64 `json:"expires_at"`
}

// admittedKey returns the per-record KV key for an admitted AK.
func admittedKey(akName []byte) string {
	sum := sha256.Sum256(akName)
	return admittedNS + hex.EncodeToString(sum[:])
}

// NewAttestationGate builds a gate over an EKRegistry + Verifier with
// the given enabled flag and TTL. A zero ttl uses DefaultAdmissionTTL.
// The admitted set is in-memory ; use NewAttestationGateWithKV to back it
// with a shared KVStorage for an HA control plane.
func NewAttestationGate(enabled bool, reg *EKRegistry, v *attest.Verifier, ttl time.Duration) *AttestationGate {
	return NewAttestationGateWithKV(enabled, reg, v, ttl, nil)
}

// NewAttestationGateWithKV is NewAttestationGate with an explicit KVStorage
// backing the admitted set. A non-nil kv makes a successful Admit visible to a
// RegisterHost served by another replica (HA) ; a nil kv keeps the admitted
// set in-memory (dev / single-process), identical to NewAttestationGate.
func NewAttestationGateWithKV(enabled bool, reg *EKRegistry, v *attest.Verifier, ttl time.Duration, kv KVStorage) *AttestationGate {
	if ttl <= 0 {
		ttl = DefaultAdmissionTTL
	}
	return &AttestationGate{
		Enabled:  enabled,
		Registry: reg,
		Verifier: v,
		ttl:      ttl,
		now:      time.Now,
		kv:       kv,
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
// gate's TTL. Called by the Admit RPC handler on a granted decision. When
// the gate is KV-backed the record is shared across replicas ; otherwise it
// lands in the in-memory map.
func (g *AttestationGate) MarkAdmitted(akName []byte) {
	deadline := g.now().Add(g.ttl)
	if g.kv != nil {
		blob, err := json.Marshal(admittedRecord{ExpiresAt: deadline.UnixNano()})
		if err == nil {
			// A persist failure leaves no admission ; the node simply re-runs
			// the handshake (fail-closed). No in-memory fallback write — that
			// would defeat the HA contract.
			_ = g.kv.PutOne(context.Background(), admittedKey(akName), blob)
		}
		return
	}
	g.mu.Lock()
	g.admitted[string(akName)] = deadline
	g.mu.Unlock()
}

// IsAdmitted reports whether akName has a non-expired admission. Expired
// entries are swept on read. An empty akName is never admitted. Reads the
// KV backend when wired, else the in-memory map.
func (g *AttestationGate) IsAdmitted(akName []byte) bool {
	if len(akName) == 0 {
		return false
	}
	if g.kv != nil {
		key := admittedKey(akName)
		blob, err := g.kv.GetOne(context.Background(), key)
		if err != nil || blob == nil {
			return false
		}
		var rec admittedRecord
		if err := json.Unmarshal(blob, &rec); err != nil {
			return false
		}
		if g.now().UnixNano() > rec.ExpiresAt {
			_ = g.kv.DeleteOne(context.Background(), key) // sweep expired
			return false
		}
		return true
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
// KV-backed = get-then-delete (single-use across replicas) ; else in-memory.
func (g *AttestationGate) ConsumeAdmission(akName []byte) bool {
	if len(akName) == 0 {
		return false
	}
	if g.kv != nil {
		key := admittedKey(akName)
		blob, err := g.kv.GetOne(context.Background(), key)
		if err != nil || blob == nil {
			return false
		}
		// Delete first : the delete is the single-use linearisation point, so
		// a racing ConsumeAdmission on another replica reads the gone key.
		_ = g.kv.DeleteOne(context.Background(), key)
		var rec admittedRecord
		if err := json.Unmarshal(blob, &rec); err != nil {
			return false
		}
		return g.now().UnixNano() <= rec.ExpiresAt
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
