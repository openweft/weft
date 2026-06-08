package weft

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeProjectRecord_RoundTrip(t *testing.T) {
	in := Project{
		UUID:         "p-1",
		Name:         "team-net",
		CreatedAt:    time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
		Members:      []string{"u-2", "u-1"}, // unsorted on input
		NATSUserSeed: "SUABCDEFG",
	}
	blob := encodeProjectRecord(in)
	got, err := decodeProjectRecord(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UUID != in.UUID || got.Name != in.Name {
		t.Errorf("uuid/name lost : got %+v ; want %+v", got, in)
	}
	if got.NATSUserSeed != in.NATSUserSeed {
		t.Errorf("seed lost : got %q ; want %q", got.NATSUserSeed, in.NATSUserSeed)
	}
	if len(got.Members) != 2 {
		t.Fatalf("members count lost : %v", got.Members)
	}
	// encode stable-sorts members ; round-trip must preserve sorted order.
	if got.Members[0] != "u-1" || got.Members[1] != "u-2" {
		t.Errorf("members not sorted on round-trip : %v", got.Members)
	}
}

func TestEncodeProjectRecord_OmitsEmptyMembersAndSeed(t *testing.T) {
	// A bare project shouldn't carry `members = []` or
	// `nats_user_seed = ""` lines — keep the HCL clean.
	in := Project{
		UUID:      "p-bare",
		Name:      "bare",
		CreatedAt: time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
	}
	blob := encodeProjectRecord(in)
	s := string(blob)
	if strings.Contains(s, "members") {
		t.Errorf("empty members produced HCL line : %q", s)
	}
	if strings.Contains(s, "nats_user_seed") {
		t.Errorf("empty nats_user_seed produced HCL line : %q", s)
	}
}

func TestProjectRegistry_KV_CreateRenameDeletePerRecord(t *testing.T) {
	kv := newMemKV("/test/projects/")
	reg, err := loadProjectRegistryKV(context.Background(), kv, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := reg.getOrCreate("alpha")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := kv.GetOne(context.Background(), p.UUID)
	if len(stored) == 0 {
		t.Fatal("getOrCreate didn't PutOne the record")
	}
	// A second create lands its own key, leaves the first alone.
	p2, _, err := reg.getOrCreate("beta")
	if err != nil {
		t.Fatal(err)
	}
	all, _ := kv.List(context.Background())
	if len(all) != 2 {
		t.Errorf("kv has %d records ; want 2", len(all))
	}

	// rename only PUTs the affected record.
	prevBlob, _ := kv.GetOne(context.Background(), p2.UUID)
	if err := reg.rename(p.UUID, "alpha-prime"); err != nil {
		t.Fatal(err)
	}
	curBlob, _ := kv.GetOne(context.Background(), p2.UUID)
	if string(prevBlob) != string(curBlob) {
		t.Error("rename on p rewrote p2 — per-record contract broken")
	}
	// The renamed record carries the new name.
	rBlob, _ := kv.GetOne(context.Background(), p.UUID)
	if !strings.Contains(string(rBlob), "alpha-prime") {
		t.Errorf("renamed record doesn't carry new name : %q", rBlob)
	}

	// addMember + removeMember are per-record.
	if err := reg.addMember(p.UUID, "u-1"); err != nil {
		t.Fatal(err)
	}
	memBlob, _ := kv.GetOne(context.Background(), p.UUID)
	if !strings.Contains(string(memBlob), "u-1") {
		t.Errorf("addMember didn't land in the per-record blob : %q", memBlob)
	}
	if err := reg.removeMember(p.UUID, "u-1"); err != nil {
		t.Fatal(err)
	}

	// delete drops only the targeted key.
	if err := reg.delete(p.UUID); err != nil {
		t.Fatal(err)
	}
	if blob, _ := kv.GetOne(context.Background(), p.UUID); blob != nil {
		t.Error("delete left the p record in the KV store")
	}
	if _, ok := reg.lookupByUUID(p2.UUID); !ok {
		t.Error("p2 fell out of the registry after deleting p")
	}
}

func TestProjectRegistry_KV_ApplyKVEventPutAndDelete(t *testing.T) {
	kv := newMemKV("/test/projects/")
	reg, _ := loadProjectRegistryKV(context.Background(), kv, nil)

	// Synthesise a remote Put event.
	remote := Project{
		UUID:      "remote-1",
		Name:      "remote-team",
		CreatedAt: time.Now().UTC(),
	}
	blob := encodeProjectRecord(remote)
	reg.applyKVEvent(KVEvent{
		Kind: KVPut, Key: "/test/projects/remote-1", Value: blob,
	})
	if got, ok := reg.lookupByUUID("remote-1"); !ok || got.Name != "remote-team" {
		t.Errorf("Put apply failed : got %+v ok=%v", got, ok)
	}
	// nameIdx populated.
	if u, ok := reg.lookupByName("remote-team"); !ok || u != "remote-1" {
		t.Errorf("nameIdx not refreshed : got %q ok=%v", u, ok)
	}

	// A Put with the same UUID but a new name should rotate the
	// nameIdx (no stale old name pointing at us).
	renamed := remote
	renamed.Name = "remote-team-v2"
	reg.applyKVEvent(KVEvent{
		Kind: KVPut, Key: "/test/projects/remote-1", Value: encodeProjectRecord(renamed),
	})
	if _, ok := reg.lookupByName("remote-team"); ok {
		t.Error("stale name still in nameIdx after remote rename")
	}
	if u, ok := reg.lookupByName("remote-team-v2"); !ok || u != "remote-1" {
		t.Errorf("new name didn't land : %q ok=%v", u, ok)
	}

	// Delete with PrevValue cleans indices.
	reg.applyKVEvent(KVEvent{
		Kind:      KVDelete,
		Key:       "/test/projects/remote-1",
		PrevValue: encodeProjectRecord(renamed),
	})
	if _, ok := reg.lookupByUUID("remote-1"); ok {
		t.Error("Delete event didn't drop the project from byUUID")
	}
	if _, ok := reg.lookupByName("remote-team-v2"); ok {
		t.Error("Delete event didn't drop the project name from nameIdx")
	}
}

func TestProjectRegistry_KV_MigrateLegacyBlobOnEmptyKV(t *testing.T) {
	// Pre-fill a blob Storage with one project, leave the KV empty.
	blobStore := NewMemStorage()
	legacyReg, _ := loadProjectRegistry(context.Background(), blobStore)
	p, _, err := legacyReg.getOrCreate("legacy-team")
	if err != nil {
		t.Fatal(err)
	}

	kv := newMemKV("/test/projects/")
	reg, err := loadProjectRegistryKV(context.Background(), kv, blobStore)
	if err != nil {
		t.Fatalf("load with migration : %v", err)
	}
	if got, ok := reg.lookupByUUID(p.UUID); !ok || got.Name != "legacy-team" {
		t.Errorf("migration didn't populate the registry : got %+v ok=%v", got, ok)
	}
	if blob, _ := kv.GetOne(context.Background(), p.UUID); len(blob) == 0 {
		t.Error("migration didn't Put the record under the KV prefix")
	}

	// A second loader on the same backends — the legacy blob is
	// now empty (we Save(nil)'d it). Migration must be a no-op,
	// no duplicate writes.
	reg2, err := loadProjectRegistryKV(context.Background(), kv, blobStore)
	if err != nil {
		t.Fatalf("second load : %v", err)
	}
	if got, ok := reg2.lookupByUUID(p.UUID); !ok || got.Name != "legacy-team" {
		t.Errorf("idempotent reload lost the record : got %+v ok=%v", got, ok)
	}
	all, _ := kv.List(context.Background())
	if len(all) != 1 {
		t.Errorf("expected 1 KV record after idempotent reload ; got %d", len(all))
	}
}
