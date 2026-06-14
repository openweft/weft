package weft

// pendingstore_kv.go is the etcd-backed implementation of the
// go-tpm2/attest PendingStore — the verifier's SINGLE-USE challenge
// state (the enrolment activation secret and the admission nonce) that
// must be shared across control-plane replicas for an HA deployment.
//
// Without this, the challenge state lives in each Verifier's in-memory
// MemPendingStore : a RequestAdmission served by dc1 stashes the nonce
// in dc1's memory, so an Admit routed to dc2 finds no pending nonce and
// fails ErrStaleNonce. Backing the state with a shared KVStorage makes
// the nonce (and the enrolment secret) visible to every replica.
//
// It mirrors the ekregistry_kv.go / hosts_kv.go KVStorage pattern : one
// etcd key per pending record, distinguished by a key namespace segment :
//
//   <prefix>pending/enroll/<hex sha256(akName)> → pendingRecord (secret)
//   <prefix>pending/nonce/<hex sha256(akName)>  → pendingRecord (nonce)
//
// Why hash the key : an AK Name is an opaque binary blob (alg||digest),
// not a safe etcd key. A SHA-256 of it is fixed-width and hex-safe ; the
// payload (secret / nonce) lives in the record value.
//
// SINGLE-USE : Take does GetOne → (expiry check) → DeleteOne, so a value
// is consumed at most once even across replicas (the delete is the
// linearisation point ; a racing second Take on another replica reads the
// already-deleted key and gets ok=false). This is the read-and-remove
// contract attest.PendingStore mandates.
//
// EXPIRY : the attest.PendingStore interface is deliberately TTL-agnostic
// (the backing store owns expiry). The KVStorage abstraction weft shares
// across registries does not expose an etcd lease handle, so expiry is
// carried as an absolute deadline INSIDE the record value : a Take that
// finds an expired record deletes it and reports ok=false. A production
// deployment on EtcdKVStorage may additionally attach an etcd lease to
// the underlying key for server-side GC of stale challenges that are
// never taken ; the value-carried deadline is the store-agnostic floor
// that also works over MemKVStorage (dev / tests). Either way the verifier
// never sees a stale challenge.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-tpm2/attest"
)

// DefaultPendingTTL bounds how long a stashed enrolment secret / admission
// nonce stays usable. Short by design : the node runs the handshake in one
// continuous exchange (Enroll→CompleteEnroll, RequestAdmission→Admit). A
// generous-but-bounded window absorbs network latency without leaving a
// stale single-use secret/nonce takeable.
const DefaultPendingTTL = 2 * time.Minute

const (
	// pendingEnrollNS / pendingNonceNS are the per-record key namespaces
	// under the store's shared KVStorage prefix.
	pendingEnrollNS = "pending/enroll/"
	pendingNonceNS  = "pending/nonce/"
)

// pendingRecord is the stored form of a single-use challenge value plus its
// absolute expiry deadline (Unix nanoseconds). The payload is the activation
// secret (enroll record) or the admission nonce (nonce record).
type pendingRecord struct {
	Value     []byte `json:"value"`
	ExpiresAt int64  `json:"expires_at"` // Unix nanoseconds
}

// EtcdPendingStore is a weft-side, KVStorage-backed attest.PendingStore. It
// satisfies the go-tpm2/attest PendingStore interface (PutEnroll / TakeEnroll
// / PutNonce / TakeNonce) over a shared KV backend so challenge state is
// visible to every control-plane replica. Safe for concurrent use (the
// backend serialises its own writes ; no in-memory index is kept because the
// store is intentionally single-use and short-lived).
type EtcdPendingStore struct {
	kv  KVStorage
	ttl time.Duration
	// now is the clock, injectable for tests (mirrors AttestationGate.now).
	now func() time.Time
}

// NewEtcdPendingStore builds an EtcdPendingStore over a KVStorage backend.
// A zero ttl uses DefaultPendingTTL. The KVStorage prefix is conventionally
// "/weft/attest/" (shared with the EK registry) ; this store namespaces its
// records under pending/enroll/ and pending/nonce/.
func NewEtcdPendingStore(kv KVStorage, ttl time.Duration) (*EtcdPendingStore, error) {
	if kv == nil {
		return nil, fmt.Errorf("pending store: nil KVStorage")
	}
	if ttl <= 0 {
		ttl = DefaultPendingTTL
	}
	return &EtcdPendingStore{kv: kv, ttl: ttl, now: time.Now}, nil
}

// pendingEnrollKey / pendingNonceKey return the per-record KV key for an AK.
func pendingEnrollKey(akName []byte) string {
	sum := sha256.Sum256(akName)
	return pendingEnrollNS + hex.EncodeToString(sum[:])
}

func pendingNonceKey(akName []byte) string {
	sum := sha256.Sum256(akName)
	return pendingNonceNS + hex.EncodeToString(sum[:])
}

// put stashes value under key with the store's TTL (overwriting any prior
// record — single pending entry per AK, matching MemPendingStore).
func (s *EtcdPendingStore) put(key string, value []byte) error {
	cp := append([]byte(nil), value...)
	rec := pendingRecord{Value: cp, ExpiresAt: s.now().Add(s.ttl).UnixNano()}
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("pending store: marshal %s: %w", key, err)
	}
	if err := s.kv.PutOne(context.Background(), key, blob); err != nil {
		return fmt.Errorf("pending store: persist %s: %w", key, err)
	}
	return nil
}

// take consumes the record at key : it reads, deletes (single-use), and
// returns the value only if the record exists and has not expired. A missing,
// corrupt, or expired record returns ok=false (an expired record is also
// deleted so it cannot accumulate).
func (s *EtcdPendingStore) take(key string) ([]byte, bool) {
	ctx := context.Background()
	blob, err := s.kv.GetOne(ctx, key)
	if err != nil || blob == nil {
		return nil, false
	}
	// Delete first so the value is consumed at most once even under a race
	// (the delete is the single-use linearisation point). Deleting an
	// already-gone key is a no-op.
	_ = s.kv.DeleteOne(ctx, key)

	var rec pendingRecord
	if err := json.Unmarshal(blob, &rec); err != nil {
		return nil, false // corrupt record : treat as absent (already deleted)
	}
	if s.now().UnixNano() > rec.ExpiresAt {
		return nil, false // expired : already deleted above
	}
	return rec.Value, true
}

// PutEnroll stashes the activation secret for akName. Implements
// attest.PendingStore.
func (s *EtcdPendingStore) PutEnroll(akName, secret []byte) error {
	return s.put(pendingEnrollKey(akName), secret)
}

// TakeEnroll consumes the activation secret for akName (single-use).
// Implements attest.PendingStore.
func (s *EtcdPendingStore) TakeEnroll(akName []byte) ([]byte, bool) {
	return s.take(pendingEnrollKey(akName))
}

// PutNonce stashes the admission nonce for akName. Implements
// attest.PendingStore.
func (s *EtcdPendingStore) PutNonce(akName, nonce []byte) error {
	return s.put(pendingNonceKey(akName), nonce)
}

// TakeNonce consumes the admission nonce for akName (single-use). Implements
// attest.PendingStore.
func (s *EtcdPendingStore) TakeNonce(akName []byte) ([]byte, bool) {
	return s.take(pendingNonceKey(akName))
}

// ensure EtcdPendingStore satisfies the attest.PendingStore interface.
var _ attest.PendingStore = (*EtcdPendingStore)(nil)
