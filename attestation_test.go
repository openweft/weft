package weft

import (
	"testing"
	"time"
)

// TestAttestationGate_DisabledByDefault asserts a nil gate and a
// zero-value / disabled gate both report not-enabled — the contract that
// keeps RegisterHost on its legacy OFF path.
func TestAttestationGate_DisabledByDefault(t *testing.T) {
	var nilGate *AttestationGate
	if nilGate.IsEnabled() {
		t.Error("nil gate reported enabled")
	}
	// A gate with enabled=false is off even with a verifier wired.
	g := NewAttestationGate(false, nil, nil, 0)
	if g.IsEnabled() {
		t.Error("disabled gate reported enabled")
	}
	// enabled=true but no verifier is still off (defensive).
	g2 := NewAttestationGate(true, nil, nil, 0)
	if g2.IsEnabled() {
		t.Error("gate with nil verifier reported enabled")
	}
}

// TestAttestationGate_AdmitConsume covers the admitted-set lifecycle :
// mark → IsAdmitted true → ConsumeAdmission true once → false thereafter.
func TestAttestationGate_AdmitConsume(t *testing.T) {
	g := NewAttestationGate(true, nil, nil, time.Minute)
	ak := []byte("ak-1")

	if g.IsAdmitted(ak) {
		t.Fatal("admitted before MarkAdmitted")
	}
	g.MarkAdmitted(ak)
	if !g.IsAdmitted(ak) {
		t.Fatal("not admitted after MarkAdmitted")
	}
	// Consume grants exactly once.
	if !g.ConsumeAdmission(ak) {
		t.Fatal("first ConsumeAdmission should succeed")
	}
	if g.ConsumeAdmission(ak) {
		t.Fatal("second ConsumeAdmission should fail (single-use)")
	}
	if g.IsAdmitted(ak) {
		t.Fatal("still admitted after consume")
	}
}

// TestAttestationGate_TTLExpiry covers expiry : an admission past its TTL
// is neither admitted nor consumable.
func TestAttestationGate_TTLExpiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := NewAttestationGate(true, nil, nil, time.Minute)
	g.now = func() time.Time { return now }
	ak := []byte("ak-ttl")
	g.MarkAdmitted(ak)

	// Advance past the TTL.
	now = now.Add(2 * time.Minute)
	if g.IsAdmitted(ak) {
		t.Error("expired admission still reported admitted")
	}
	// Re-mark, then expire, then consume must fail.
	now = time.Unix(2_000_000, 0)
	g.MarkAdmitted(ak)
	now = now.Add(90 * time.Second)
	if g.ConsumeAdmission(ak) {
		t.Error("expired admission was consumable")
	}
}

// TestAttestationGate_EmptyAK rejects empty AK names everywhere.
func TestAttestationGate_EmptyAK(t *testing.T) {
	g := NewAttestationGate(true, nil, nil, 0)
	if g.IsAdmitted(nil) {
		t.Error("empty AK reported admitted")
	}
	if g.ConsumeAdmission(nil) {
		t.Error("empty AK consumed")
	}
	// Marking empty then querying empty stays false (len-0 guard).
	g.MarkAdmitted([]byte{})
	if g.IsAdmitted([]byte{}) {
		t.Error("empty AK admitted after mark")
	}
}

// TestAttestationGate_DefaultTTL covers the zero-ttl → DefaultAdmissionTTL
// fallback.
func TestAttestationGate_DefaultTTL(t *testing.T) {
	g := NewAttestationGate(true, nil, nil, 0)
	if g.ttl != DefaultAdmissionTTL {
		t.Fatalf("ttl = %v ; want default %v", g.ttl, DefaultAdmissionTTL)
	}
}
