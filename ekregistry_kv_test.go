package weft

import (
	"context"
	"encoding/json"
	"testing"
)

// TestEKRegistry_TrustAndQuery covers the Trusted predicate over the
// TrustEK provisioning call : an untrusted EK reports false, a trusted
// one true, and trust is keyed on the exact EK bytes (a different blob
// is not trusted).
func TestEKRegistry_TrustAndQuery(t *testing.T) {
	kv := newMemKV("/test/attest/")
	reg, err := LoadEKRegistry(context.Background(), kv)
	if err != nil {
		t.Fatal(err)
	}
	ek := []byte("ek-public-area-bytes")
	if reg.Trusted(ek) {
		t.Fatal("EK trusted before TrustEK")
	}
	if err := reg.TrustEK(ek); err != nil {
		t.Fatalf("TrustEK: %v", err)
	}
	if !reg.Trusted(ek) {
		t.Fatal("EK not trusted after TrustEK")
	}
	if reg.Trusted([]byte("a-different-ek")) {
		t.Fatal("a different EK was wrongly trusted")
	}
	// The trusted EK persisted to the KV under the ek/ namespace.
	if blob, _ := kv.GetOne(context.Background(), ekRecordKey(ek)); len(blob) == 0 {
		t.Fatal("TrustEK did not persist a record")
	}
}

// TestEKRegistry_BindAndAKPub covers the BindAK → AKPub round-trip and
// that AKPub returns false for an unbound AK + a defensive copy.
func TestEKRegistry_BindAndAKPub(t *testing.T) {
	kv := newMemKV("/test/attest/")
	reg, _ := LoadEKRegistry(context.Background(), kv)

	akName := []byte("ak-name-alg-digest")
	akPub := []byte("ak-public-area")

	if _, ok := reg.AKPub(akName); ok {
		t.Fatal("AKPub returned ok for unbound AK")
	}
	if err := reg.BindAK(akName, akPub); err != nil {
		t.Fatalf("BindAK: %v", err)
	}
	got, ok := reg.AKPub(akName)
	if !ok {
		t.Fatal("AKPub not ok after BindAK")
	}
	if string(got) != string(akPub) {
		t.Fatalf("AKPub = %q ; want %q", got, akPub)
	}
	// Defensive copy : mutating the returned slice must not corrupt the
	// stored value.
	got[0] = 'X'
	again, _ := reg.AKPub(akName)
	if string(again) != string(akPub) {
		t.Fatalf("AKPub mutated through returned slice : %q", again)
	}
	if blob, _ := kv.GetOne(context.Background(), akRecordKey(akName)); len(blob) == 0 {
		t.Fatal("BindAK did not persist a record")
	}
}

// TestEKRegistry_ReloadFromKV proves the in-memory index is rebuilt from
// the KV on a fresh Load — the persisted trust + bindings survive a
// restart (control-plane process bounce).
func TestEKRegistry_ReloadFromKV(t *testing.T) {
	kv := newMemKV("/test/attest/")
	reg, _ := LoadEKRegistry(context.Background(), kv)
	ek := []byte("durable-ek")
	akName := []byte("durable-ak-name")
	akPub := []byte("durable-ak-pub")
	if err := reg.TrustEK(ek); err != nil {
		t.Fatal(err)
	}
	if err := reg.BindAK(akName, akPub); err != nil {
		t.Fatal(err)
	}

	// Fresh registry over the SAME KV : simulates a restart.
	reg2, err := LoadEKRegistry(context.Background(), kv)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reg2.Trusted(ek) {
		t.Error("trusted EK not restored on reload")
	}
	if got, ok := reg2.AKPub(akName); !ok || string(got) != string(akPub) {
		t.Errorf("bound AK not restored on reload : %q ok=%v", got, ok)
	}
}

// TestEKRegistry_NilKV rejects construction without a backend.
func TestEKRegistry_NilKV(t *testing.T) {
	if _, err := LoadEKRegistry(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil KVStorage")
	}
}

// TestEKRegistry_ApplyKVEvent covers the multi-replica catch-up hook :
// a remote Put (EK + AK) lands in the index, and a remote AK Delete
// (carrying PrevValue) drops it.
func TestEKRegistry_ApplyKVEvent(t *testing.T) {
	kv := newMemKV("/test/attest/")
	reg, _ := LoadEKRegistry(context.Background(), kv)

	ek := []byte("remote-ek")
	ekBlob, _ := json.Marshal(trustedEKRecord{EKPub: ek})
	reg.applyEKKVEvent(KVEvent{Kind: KVPut, Key: "/test/attest/" + ekRecordKey(ek), Value: ekBlob})
	if !reg.Trusted(ek) {
		t.Error("remote EK Put not applied")
	}

	akName := []byte("remote-ak")
	akPub := []byte("remote-ak-pub")
	akBlob, _ := json.Marshal(boundAKRecord{AKName: akName, AKPub: akPub})
	reg.applyEKKVEvent(KVEvent{Kind: KVPut, Key: "/test/attest/" + akRecordKey(akName), Value: akBlob})
	if _, ok := reg.AKPub(akName); !ok {
		t.Error("remote AK Put not applied")
	}

	// Remote AK delete with PrevValue cleans the index.
	reg.applyEKKVEvent(KVEvent{Kind: KVDelete, Key: "/test/attest/" + akRecordKey(akName), PrevValue: akBlob})
	if _, ok := reg.AKPub(akName); ok {
		t.Error("remote AK Delete did not drop the binding")
	}

	// Remote EK delete drops the trust.
	reg.applyEKKVEvent(KVEvent{Kind: KVDelete, Key: "/test/attest/" + ekRecordKey(ek)})
	if reg.Trusted(ek) {
		t.Error("remote EK Delete did not drop the trust")
	}
}
