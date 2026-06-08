package weft

import (
	"context"
	"testing"
	"time"
)

func TestEncodeDecodeHostRecord_RoundTrip(t *testing.T) {
	in := Host{
		UUID:       "h-1",
		Hostname:   "compute-01",
		AZ:         "us-east-1a",
		Rack:       "r1",
		Endpoint:   "compute-01.internal:8443",
		Hypervisor: "qemu",
		Architecture: "arm64",
		State:      HostStateActive,
		LastSeenAt: time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
		CreatedAt:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Labels:     map[string]string{"gpu": "h200", "tier": "prod"},
	}
	blob := encodeHostRecord(in)
	got, err := decodeHostRecord(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UUID != in.UUID || got.Hostname != in.Hostname || got.AZ != in.AZ {
		t.Errorf("got %+v ; want %+v", got, in)
	}
	if got.State != HostStateActive {
		t.Errorf("state = %q ; want active", got.State)
	}
	if got.Labels["gpu"] != "h200" {
		t.Errorf("labels lost : %v", got.Labels)
	}
}

func TestHostRegistry_KV_CreateAndDeletePerRecord(t *testing.T) {
	kv := newMemKV("/test/hosts/")
	reg, err := loadHostRegistryKV(context.Background(), kv, nil)
	if err != nil {
		t.Fatal(err)
	}
	h1, err := reg.register(RegisterHostSpec{
		Hostname: "h1", Endpoint: "h1:1", Hypervisor: "qemu", Architecture: "arm64",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Per-record key was created.
	if blob, _ := kv.GetOne(context.Background(), h1.UUID); len(blob) == 0 {
		t.Fatal("register didn't PutOne")
	}
	// Second host doesn't touch first.
	h2, _ := reg.register(RegisterHostSpec{
		Hostname: "h2", Endpoint: "h2:1", Hypervisor: "qemu", Architecture: "arm64",
	})
	prev, _ := kv.GetOne(context.Background(), h1.UUID)
	if err := reg.setLabels(h2.UUID, map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := kv.GetOne(context.Background(), h1.UUID)
	if string(prev) != string(cur) {
		t.Error("setLabels on h2 rewrote h1 — per-record contract broken")
	}
	// Delete : refused while active.
	if err := reg.delete(h1.UUID); err == nil {
		t.Error("delete of active host should be rejected")
	}
	_ = reg.setState(h1.UUID, HostStateDown)
	if err := reg.delete(h1.UUID); err != nil {
		t.Fatal(err)
	}
	if blob, _ := kv.GetOne(context.Background(), h1.UUID); blob != nil {
		t.Error("delete left record in KV")
	}
}

func TestHostRegistry_KV_ApplyKVEvent(t *testing.T) {
	kv := newMemKV("/test/hosts/")
	reg, _ := loadHostRegistryKV(context.Background(), kv, nil)

	// Synthesise a remote Put.
	remote := Host{
		UUID: "h-remote", Hostname: "dc2", Endpoint: "dc2:1",
		Hypervisor: "qemu", Architecture: "arm64",
		State: HostStateActive, CreatedAt: time.Now().UTC(),
	}
	reg.applyKVEvent(KVEvent{
		Kind: KVPut, Key: "/test/hosts/h-remote", Value: encodeHostRecord(remote),
	})
	if got, ok := reg.lookupByUUID("h-remote"); !ok || got.Hostname != "dc2" {
		t.Errorf("remote Put not applied : %+v ok=%v", got, ok)
	}
	if _, ok := reg.lookupByHostname("dc2"); !ok {
		t.Error("nameIdx missing for remote Put")
	}

	// Delete with PrevValue cleans both indices.
	reg.applyKVEvent(KVEvent{
		Kind: KVDelete, Key: "/test/hosts/h-remote", PrevValue: encodeHostRecord(remote),
	})
	if _, ok := reg.lookupByUUID("h-remote"); ok {
		t.Error("Delete didn't drop byUUID")
	}
	if _, ok := reg.lookupByHostname("dc2"); ok {
		t.Error("Delete didn't clean nameIdx")
	}
}

func TestHostRegistry_KV_MigrateLegacyBlob(t *testing.T) {
	// Pre-fill legacy blob with a host.
	blobStore := NewMemStorage()
	legacyReg, _ := loadHostRegistry(context.Background(), blobStore)
	h, _ := legacyReg.register(RegisterHostSpec{
		Hostname: "legacy", Endpoint: "leg:1", Hypervisor: "qemu", Architecture: "arm64",
	})

	kv := newMemKV("/test/hosts/")
	reg, err := loadHostRegistryKV(context.Background(), kv, blobStore)
	if err != nil {
		t.Fatalf("load with migration: %v", err)
	}
	if got, ok := reg.lookupByUUID(h.UUID); !ok || got.Hostname != "legacy" {
		t.Errorf("migration didn't populate registry : %+v ok=%v", got, ok)
	}
	if blob, _ := kv.GetOne(context.Background(), h.UUID); len(blob) == 0 {
		t.Error("migration didn't put record in KV")
	}
}
