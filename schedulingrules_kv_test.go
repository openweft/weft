package weft

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeSchedulingRuleRecord_RoundTrip(t *testing.T) {
	in := SchedulingRuleEntry{
		UUID:         "sr-1",
		Name:         "pin-web",
		Selector:     "vm.name=web",
		TargetCount:  3,
		AntiAffinity: "host",
		CreatedAt:    time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
		Respawn: &RespawnPolicyJSON{
			Enabled:       true,
			GracePeriodMs: 5_000,
			MaxRestarts:   3,
			WindowMs:      60_000,
			Backoff:       "exponential",
			Liveness: &HealthProbeJSON{
				Type:     "http",
				HttpPath: "/healthz",
				HttpPort: 8080,
			},
		},
	}
	blob := encodeSchedulingRuleRecord(in)
	got, err := decodeSchedulingRuleRecord(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UUID != in.UUID || got.Name != in.Name {
		t.Errorf("uuid/name lost : %+v", got)
	}
	if got.Selector != in.Selector || got.TargetCount != in.TargetCount {
		t.Errorf("selector/target_count lost : %+v", got)
	}
	if got.AntiAffinity != in.AntiAffinity {
		t.Errorf("anti_affinity lost : %q", got.AntiAffinity)
	}
	if got.Respawn == nil {
		t.Fatal("respawn lost on round-trip")
	}
	if !got.Respawn.Enabled || got.Respawn.MaxRestarts != 3 {
		t.Errorf("respawn fields lost : %+v", got.Respawn)
	}
	if got.Respawn.Liveness == nil || got.Respawn.Liveness.HttpPath != "/healthz" {
		t.Errorf("nested health probe lost : %+v", got.Respawn.Liveness)
	}
}

func TestEncodeSchedulingRuleRecord_OmitsEmptyRespawn(t *testing.T) {
	in := SchedulingRuleEntry{
		UUID:      "sr-bare",
		Name:      "bare",
		CreatedAt: time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
	}
	blob := encodeSchedulingRuleRecord(in)
	if strings.Contains(string(blob), "respawn_json") {
		t.Errorf("empty respawn produced HCL line : %q", blob)
	}
}

func TestSchedulingRuleRegistry_KV_CreateUpdateDeletePerRecord(t *testing.T) {
	kv := newMemKV("/test/scheduling_rules/")
	reg, err := loadSchedulingRuleRegistryKV(context.Background(), kv, nil)
	if err != nil {
		t.Fatal(err)
	}
	sr, _, err := reg.create("pin-web", "vm.name=web", 3, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := kv.GetOne(context.Background(), sr.UUID)
	if len(stored) == 0 {
		t.Fatal("create didn't PutOne the record")
	}
	// A second create lands its own key, leaves the first alone.
	sr2, _, err := reg.create("pin-db", "vm.name=db", 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := kv.List(context.Background())
	if len(all) != 2 {
		t.Errorf("kv has %d records ; want 2", len(all))
	}

	// update only PUTs the affected record.
	prevBlob, _ := kv.GetOne(context.Background(), sr2.UUID)
	if _, err := reg.update(sr.UUID, "", 5, "", nil, false); err != nil {
		t.Fatal(err)
	}
	curBlob, _ := kv.GetOne(context.Background(), sr2.UUID)
	if string(prevBlob) != string(curBlob) {
		t.Error("update on sr rewrote sr2 — per-record contract broken")
	}
	rBlob, _ := kv.GetOne(context.Background(), sr.UUID)
	if !strings.Contains(string(rBlob), "target_count = 5") {
		t.Errorf("updated record doesn't carry new target_count : %q", rBlob)
	}

	// delete drops only the targeted key.
	if err := reg.delete(sr.UUID); err != nil {
		t.Fatal(err)
	}
	if blob, _ := kv.GetOne(context.Background(), sr.UUID); blob != nil {
		t.Error("delete left the sr record in the KV store")
	}
	if _, ok := reg.lookupByUUID(sr2.UUID); !ok {
		t.Error("sr2 fell out of the registry after deleting sr")
	}
}

func TestSchedulingRuleRegistry_KV_ApplyKVEventPutAndDelete(t *testing.T) {
	kv := newMemKV("/test/scheduling_rules/")
	reg, _ := loadSchedulingRuleRegistryKV(context.Background(), kv, nil)

	// Synthesise a remote Put event.
	remote := SchedulingRuleEntry{
		UUID:         "remote-1",
		Name:         "remote-pin",
		Selector:     "vm.name=remote",
		TargetCount:  2,
		CreatedAt:    time.Now().UTC(),
	}
	blob := encodeSchedulingRuleRecord(remote)
	reg.applyKVEvent(KVEvent{
		Kind: KVPut, Key: "/test/scheduling_rules/remote-1", Value: blob,
	})
	if got, ok := reg.lookupByUUID("remote-1"); !ok || got.Name != "remote-pin" {
		t.Errorf("Put apply failed : got %+v ok=%v", got, ok)
	}

	// A Put rotating the name should clean stale nameIdx.
	renamed := remote
	renamed.Name = "remote-pin-v2"
	reg.applyKVEvent(KVEvent{
		Kind: KVPut, Key: "/test/scheduling_rules/remote-1", Value: encodeSchedulingRuleRecord(renamed),
	})
	if _, ok := findRuleByName(reg, "remote-pin"); ok {
		t.Error("stale name still in nameIdx after remote rename")
	}
	if u, ok := findRuleByName(reg, "remote-pin-v2"); !ok || u != "remote-1" {
		t.Errorf("new name didn't land : %q ok=%v", u, ok)
	}

	// Delete with PrevValue cleans indices.
	reg.applyKVEvent(KVEvent{
		Kind:      KVDelete,
		Key:       "/test/scheduling_rules/remote-1",
		PrevValue: encodeSchedulingRuleRecord(renamed),
	})
	if _, ok := reg.lookupByUUID("remote-1"); ok {
		t.Error("Delete event didn't drop the rule from byUUID")
	}
	if _, ok := findRuleByName(reg, "remote-pin-v2"); ok {
		t.Error("Delete event didn't drop the rule name from nameIdx")
	}
}

func TestSchedulingRuleRegistry_KV_MigrateLegacyBlobOnEmptyKV(t *testing.T) {
	// Pre-fill a blob Storage with one rule (legacy JSON shape).
	blobStore := NewMemStorage()
	legacyReg, _ := loadSchedulingRuleRegistry(context.Background(), blobStore)
	sr, _, err := legacyReg.create("legacy-rule", "vm.name=foo", 2, "rack", &RespawnPolicyJSON{
		Enabled: true, MaxRestarts: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	kv := newMemKV("/test/scheduling_rules/")
	reg, err := loadSchedulingRuleRegistryKV(context.Background(), kv, blobStore)
	if err != nil {
		t.Fatalf("load with migration : %v", err)
	}
	got, ok := reg.lookupByUUID(sr.UUID)
	if !ok || got.Name != "legacy-rule" {
		t.Errorf("migration didn't populate the registry : got %+v ok=%v", got, ok)
	}
	if got.Respawn == nil || got.Respawn.MaxRestarts != 5 {
		t.Errorf("respawn lost on migration : %+v", got.Respawn)
	}
	if blob, _ := kv.GetOne(context.Background(), sr.UUID); len(blob) == 0 {
		t.Error("migration didn't Put the record under the KV prefix")
	}

	// Idempotent second loader : legacy blob is empty now, KV
	// already populated, no duplicates.
	reg2, err := loadSchedulingRuleRegistryKV(context.Background(), kv, blobStore)
	if err != nil {
		t.Fatalf("second load : %v", err)
	}
	if got2, ok := reg2.lookupByUUID(sr.UUID); !ok || got2.Name != "legacy-rule" {
		t.Errorf("idempotent reload lost the record : got %+v ok=%v", got2, ok)
	}
	all, _ := kv.List(context.Background())
	if len(all) != 1 {
		t.Errorf("expected 1 KV record after idempotent reload ; got %d", len(all))
	}
}

// findRuleByName scans the registry for a rule with the given name.
// The schedulingRuleRegistry doesn't expose a public name lookup
// (the gRPC layer uses UUIDs end-to-end) so the test reaches into
// the nameIdx directly via the same lock the public methods use.
func findRuleByName(r *schedulingRuleRegistry, name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.nameIdx[name]
	return u, ok
}
