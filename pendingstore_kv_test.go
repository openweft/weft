package weft

import (
	"context"
	"testing"
	"time"

	"github.com/go-tpm2/attest"
)

// TestEtcdPendingStore_NilKV rejects a nil backend.
func TestEtcdPendingStore_NilKV(t *testing.T) {
	if _, err := NewEtcdPendingStore(nil, time.Minute); err == nil {
		t.Fatal("expected error for nil KVStorage")
	}
}

// TestEtcdPendingStore_DefaultTTL covers the zero-ttl → DefaultPendingTTL
// fallback.
func TestEtcdPendingStore_DefaultTTL(t *testing.T) {
	s, err := NewEtcdPendingStore(newMemKV("/test/attest/"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ttl != DefaultPendingTTL {
		t.Fatalf("ttl = %v ; want default %v", s.ttl, DefaultPendingTTL)
	}
	// And it satisfies the attest.PendingStore interface.
	var _ attest.PendingStore = s
}

// TestEtcdPendingStore_EnrollSingleUse covers the enrol secret lifecycle :
// put → take once → take again fails (single-use), and a missing entry is
// ok=false.
func TestEtcdPendingStore_EnrollSingleUse(t *testing.T) {
	s, _ := NewEtcdPendingStore(newMemKV("/test/attest/"), time.Minute)
	ak := []byte("ak-enroll")

	if _, ok := s.TakeEnroll(ak); ok {
		t.Fatal("TakeEnroll on empty store should be ok=false")
	}
	if err := s.PutEnroll(ak, []byte("secret-1")); err != nil {
		t.Fatalf("PutEnroll: %v", err)
	}
	// Overwrite-prior semantics.
	if err := s.PutEnroll(ak, []byte("secret-2")); err != nil {
		t.Fatalf("PutEnroll overwrite: %v", err)
	}
	got, ok := s.TakeEnroll(ak)
	if !ok || string(got) != "secret-2" {
		t.Fatalf("TakeEnroll got %q ok=%v want secret-2 true", got, ok)
	}
	if _, ok := s.TakeEnroll(ak); ok {
		t.Fatal("second TakeEnroll should be ok=false (single-use)")
	}
}

// TestEtcdPendingStore_NonceSingleUse mirrors the above for the nonce path.
func TestEtcdPendingStore_NonceSingleUse(t *testing.T) {
	s, _ := NewEtcdPendingStore(newMemKV("/test/attest/"), time.Minute)
	ak := []byte("ak-nonce")

	if _, ok := s.TakeNonce(ak); ok {
		t.Fatal("TakeNonce on empty store should be ok=false")
	}
	if err := s.PutNonce(ak, []byte("nonce-1")); err != nil {
		t.Fatalf("PutNonce: %v", err)
	}
	got, ok := s.TakeNonce(ak)
	if !ok || string(got) != "nonce-1" {
		t.Fatalf("TakeNonce got %q ok=%v want nonce-1 true", got, ok)
	}
	if _, ok := s.TakeNonce(ak); ok {
		t.Fatal("second TakeNonce should be ok=false (single-use)")
	}
}

// TestEtcdPendingStore_Expiry covers TTL expiry : a value taken after its
// deadline is ok=false (and swept).
func TestEtcdPendingStore_Expiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s, _ := NewEtcdPendingStore(newMemKV("/test/attest/"), time.Minute)
	s.now = func() time.Time { return now }
	ak := []byte("ak-expiry")

	if err := s.PutEnroll(ak, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutNonce(ak, []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // past TTL

	if _, ok := s.TakeEnroll(ak); ok {
		t.Error("expired enroll secret was takeable")
	}
	if _, ok := s.TakeNonce(ak); ok {
		t.Error("expired nonce was takeable")
	}
}

// TestEtcdPendingStore_CorruptRecord covers a malformed stored blob :
// take treats it as absent (and deletes it).
func TestEtcdPendingStore_CorruptRecord(t *testing.T) {
	kv := newMemKV("/test/attest/")
	s, _ := NewEtcdPendingStore(kv, time.Minute)
	ak := []byte("ak-corrupt")

	// Write a non-JSON blob directly under the enroll key.
	if err := kv.PutOne(nil, pendingEnrollKey(ak), []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.TakeEnroll(ak); ok {
		t.Fatal("corrupt record should be ok=false")
	}
}

// failKV is a KVStorage whose PutOne always fails, to drive the store-write
// error branches. All other methods delegate to an embedded memKV.
type failKV struct {
	*memKV
}

func (failKV) PutOne(_ context.Context, _ string, _ []byte) error {
	return errKVDown
}

var errKVDown = errKVDownErr("kv put failed")

type errKVDownErr string

func (e errKVDownErr) Error() string { return string(e) }

// TestEtcdPendingStore_PutError surfaces a KV PutOne failure as a Put error
// (the challenge could not be persisted).
func TestEtcdPendingStore_PutError(t *testing.T) {
	s, _ := NewEtcdPendingStore(failKV{newMemKV("/test/attest/")}, time.Minute)
	if err := s.PutEnroll([]byte("ak"), []byte("s")); err == nil {
		t.Fatal("expected PutEnroll error on KV failure")
	}
	if err := s.PutNonce([]byte("ak"), []byte("n")); err == nil {
		t.Fatal("expected PutNonce error on KV failure")
	}
}

// TestAttestationGate_KVMarkPutError covers MarkAdmitted swallowing a KV
// PutOne failure (fail-closed : no admission recorded, no panic).
func TestAttestationGate_KVMarkPutError(t *testing.T) {
	kv := failKV{newMemKV("/weft/attest/")}
	g := NewAttestationGateWithKV(true, nil, nil, time.Minute, kv)
	ak := []byte("ak-mark-fail")
	g.MarkAdmitted(ak) // must not panic
	// The underlying put failed, so the AK is not admitted.
	if g.IsAdmitted(ak) {
		t.Fatal("AK reported admitted despite KV put failure")
	}
}

// TestEtcdPendingStore_HA is the core HA property : two EtcdPendingStore
// instances over the SAME KVStorage backend — a Put via instance A is
// Take-able via instance B (the value crossed the shared store), and the
// value is single-use (a second Take on EITHER instance fails).
func TestEtcdPendingStore_HA(t *testing.T) {
	kv := newMemKV("/weft/attest/") // the shared etcd-like backend
	a, _ := NewEtcdPendingStore(kv, time.Minute)
	b, _ := NewEtcdPendingStore(kv, time.Minute)
	ak := []byte("ak-ha")

	// --- Enrol secret : stash on A, consume on B. ---
	if err := a.PutEnroll(ak, []byte("shared-secret")); err != nil {
		t.Fatalf("A.PutEnroll: %v", err)
	}
	got, ok := b.TakeEnroll(ak)
	if !ok || string(got) != "shared-secret" {
		t.Fatalf("B.TakeEnroll got %q ok=%v want shared-secret true", got, ok)
	}
	// Single-use across replicas : A also sees it gone.
	if _, ok := a.TakeEnroll(ak); ok {
		t.Fatal("A.TakeEnroll after B consumed should be ok=false (single-use across replicas)")
	}

	// --- Admission nonce : stash on A, consume on B. ---
	if err := a.PutNonce(ak, []byte("shared-nonce")); err != nil {
		t.Fatalf("A.PutNonce: %v", err)
	}
	gotN, ok := b.TakeNonce(ak)
	if !ok || string(gotN) != "shared-nonce" {
		t.Fatalf("B.TakeNonce got %q ok=%v want shared-nonce true", gotN, ok)
	}
	if _, ok := b.TakeNonce(ak); ok {
		t.Fatal("second B.TakeNonce should be ok=false (single-use)")
	}
}

// TestAttestationGate_KVAdmittedHA is the admitted-set HA property : two
// AttestationGate instances over the SAME KVStorage — a MarkAdmitted on
// gate A is visible to IsAdmitted on gate B, and ConsumeAdmission on B
// succeeds exactly once (an Admit on one replica grants a RegisterHost on
// another, and only once).
func TestAttestationGate_KVAdmittedHA(t *testing.T) {
	kv := newMemKV("/weft/attest/")
	a := NewAttestationGateWithKV(true, nil, nil, time.Minute, kv)
	b := NewAttestationGateWithKV(true, nil, nil, time.Minute, kv)
	ak := []byte("ak-admit-ha")

	if b.IsAdmitted(ak) {
		t.Fatal("admitted before any MarkAdmitted")
	}
	// Replica A admits ; replica B must see it.
	a.MarkAdmitted(ak)
	if !b.IsAdmitted(ak) {
		t.Fatal("MarkAdmitted on A not visible to IsAdmitted on B")
	}
	// Replica B consumes the admission for its RegisterHost — once.
	if !b.ConsumeAdmission(ak) {
		t.Fatal("B.ConsumeAdmission should succeed for an A-admitted AK")
	}
	if a.ConsumeAdmission(ak) {
		t.Fatal("A.ConsumeAdmission after B consumed should fail (single-use across replicas)")
	}
	if b.IsAdmitted(ak) {
		t.Fatal("still admitted after consume")
	}
}

// TestAttestationGate_KVAdmittedExpiry covers KV-backed admitted-set TTL
// expiry on both the IsAdmitted sweep and the ConsumeAdmission path.
func TestAttestationGate_KVAdmittedExpiry(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	kv := newMemKV("/weft/attest/")
	g := NewAttestationGateWithKV(true, nil, nil, time.Minute, kv)
	g.now = func() time.Time { return now }
	ak := []byte("ak-admit-ttl")

	g.MarkAdmitted(ak)
	now = now.Add(2 * time.Minute) // past TTL
	if g.IsAdmitted(ak) {
		t.Error("expired admission still reported admitted (IsAdmitted)")
	}

	// Re-mark, expire, then ConsumeAdmission must fail.
	now = time.Unix(6_000_000, 0)
	g.MarkAdmitted(ak)
	now = now.Add(90 * time.Second)
	if g.ConsumeAdmission(ak) {
		t.Error("expired admission was consumable (ConsumeAdmission)")
	}
}

// TestAttestationGate_KVEmptyAK rejects empty AK names on the KV path too.
func TestAttestationGate_KVEmptyAK(t *testing.T) {
	g := NewAttestationGateWithKV(true, nil, nil, time.Minute, newMemKV("/weft/attest/"))
	if g.IsAdmitted(nil) {
		t.Error("empty AK reported admitted (KV)")
	}
	if g.ConsumeAdmission(nil) {
		t.Error("empty AK consumed (KV)")
	}
}

// TestAttestationGate_KVCorruptAdmitted covers a malformed admitted record :
// IsAdmitted / ConsumeAdmission treat it as not-admitted.
func TestAttestationGate_KVCorruptAdmitted(t *testing.T) {
	kv := newMemKV("/weft/attest/")
	g := NewAttestationGateWithKV(true, nil, nil, time.Minute, kv)
	ak := []byte("ak-admit-corrupt")
	if err := kv.PutOne(nil, admittedKey(ak), []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	if g.IsAdmitted(ak) {
		t.Error("corrupt admitted record reported admitted")
	}
	// Re-seed (the IsAdmitted above is read-only on corrupt; Consume reads its own).
	if err := kv.PutOne(nil, admittedKey(ak), []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	if g.ConsumeAdmission(ak) {
		t.Error("corrupt admitted record was consumable")
	}
}
