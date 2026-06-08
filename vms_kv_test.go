package weft

import (
	"context"
	"sync"
	"testing"
	"time"
)

// memKV is a tiny in-process KVStorage for unit tests. Doesn't dial
// etcd ; the per-record semantics are pure data so a map is enough.
type memKV struct {
	mu     sync.Mutex
	prefix string
	data   map[string][]byte
	subs   []chan KVEvent
}

func newMemKV(prefix string) *memKV {
	return &memKV{prefix: prefix, data: make(map[string][]byte)}
}

func (m *memKV) Prefix() string { return m.prefix }

func (m *memKV) GetOne(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[key], nil
}

func (m *memKV) PutOne(_ context.Context, key string, blob []byte) error {
	m.mu.Lock()
	cp := append([]byte(nil), blob...)
	m.data[key] = cp
	subs := append([]chan KVEvent(nil), m.subs...)
	m.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- KVEvent{Kind: KVPut, Key: m.prefix + key, Value: cp}:
		default:
		}
	}
	return nil
}

func (m *memKV) DeleteOne(_ context.Context, key string) error {
	m.mu.Lock()
	prev := m.data[key]
	delete(m.data, key)
	subs := append([]chan KVEvent(nil), m.subs...)
	m.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- KVEvent{Kind: KVDelete, Key: m.prefix + key, PrevValue: prev}:
		default:
		}
	}
	return nil
}

func (m *memKV) List(_ context.Context) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]byte, len(m.data))
	for k, v := range m.data {
		cp := append([]byte(nil), v...)
		out[k] = cp
	}
	return out, nil
}

func (m *memKV) WatchKeys(ctx context.Context) <-chan KVEvent {
	ch := make(chan KVEvent, 16)
	m.mu.Lock()
	m.subs = append(m.subs, ch)
	m.mu.Unlock()
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		for i, c := range m.subs {
			if c == ch {
				m.subs = append(m.subs[:i], m.subs[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
		close(ch)
	}()
	return ch
}

func TestEncodeDecodeVMRecord_RoundTrip(t *testing.T) {
	in := VM{
		UUID:         "abc-123",
		ProjectUUID:  "p-1",
		Name:         "web",
		HostUUID:     "h-prod-01",
		Image:        "ghcr.io/foo:v1",
		CPUCount:     4,
		MemoryMiB:    8192,
		Architecture: "arm64",
		State:        VMStateRunning,
		CreatedAt:    time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
		LastStartAt:  time.Date(2026, 6, 8, 13, 0, 0, 0, time.UTC),
	}
	blob := encodeVMRecord(in)
	got, err := decodeVMRecord(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UUID != in.UUID || got.Name != in.Name || got.HostUUID != in.HostUUID {
		t.Errorf("got %+v ; want %+v", got, in)
	}
	if got.CPUCount != 4 || got.MemoryMiB != 8192 || got.State != VMStateRunning {
		t.Errorf("numeric/state fields lost : %+v", got)
	}
	if got.Architecture != "arm64" {
		t.Errorf("architecture = %q ; want arm64", got.Architecture)
	}
}

func TestVMRegistry_KV_CreateUpdateDeletePerRecord(t *testing.T) {
	kv := newMemKV("/test/vms/")
	reg, err := loadVMRegistryKV(context.Background(), kv, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := reg.create(CreateVMSpec{
		ProjectUUID: "p-1", Name: "web", HostUUID: "h-01",
		Image: "img:v1", CPUCount: 2, MemoryMiB: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Per-record key landed under the prefix.
	stored, _ := kv.GetOne(context.Background(), v.UUID)
	if len(stored) == 0 {
		t.Fatal("create didn't PutOne the record")
	}
	// Another VM keeps prior records.
	v2, _ := reg.create(CreateVMSpec{
		ProjectUUID: "p-1", Name: "db", HostUUID: "h-02",
	})
	all, _ := kv.List(context.Background())
	if len(all) != 2 {
		t.Errorf("kv has %d records ; want 2", len(all))
	}

	// setHost only PUTs the affected record.
	prevBlob, _ := kv.GetOne(context.Background(), v2.UUID)
	if err := reg.setHost(v.UUID, "h-99"); err != nil {
		t.Fatal(err)
	}
	curBlob, _ := kv.GetOne(context.Background(), v2.UUID)
	if string(prevBlob) != string(curBlob) {
		t.Error("setHost on v rewrote v2 — per-record contract broken")
	}

	// delete removes only the targeted key.
	if err := reg.delete(v.UUID); err != nil {
		t.Fatal(err)
	}
	if blob, _ := kv.GetOne(context.Background(), v.UUID); blob != nil {
		t.Error("delete left the v record in the KV store")
	}
	if _, ok := reg.lookupByUUID(v2.UUID); !ok {
		t.Error("v2 fell out of the registry after deleting v")
	}
}

func TestVMRegistry_KV_ApplyKVEventPutAndDelete(t *testing.T) {
	kv := newMemKV("/test/vms/")
	reg, _ := loadVMRegistryKV(context.Background(), kv, nil)

	// Synthesise a remote Put event (e.g. another DC wrote a record).
	remoteVM := VM{
		UUID: "remote-1", ProjectUUID: "p-r", Name: "remote-web",
		HostUUID: "h-remote", State: VMStateRunning,
		CreatedAt: time.Now().UTC(),
	}
	blob := encodeVMRecord(remoteVM)
	reg.applyKVEvent(KVEvent{
		Kind: KVPut, Key: "/test/vms/remote-1", Value: blob,
	})
	if got, ok := reg.lookupByUUID("remote-1"); !ok || got.Name != "remote-web" {
		t.Errorf("Put apply failed : got %+v ok=%v", got, ok)
	}
	// hostIdx was rebuilt for the new host.
	if l := reg.listForHost("h-remote"); len(l) != 1 {
		t.Errorf("hostIdx didn't pick up the Put : got %d ; want 1", len(l))
	}

	// Now Delete (carrying PrevValue per WithPrevKV) and confirm
	// indices are cleaned.
	reg.applyKVEvent(KVEvent{
		Kind: KVDelete, Key: "/test/vms/remote-1", PrevValue: blob,
	})
	if _, ok := reg.lookupByUUID("remote-1"); ok {
		t.Error("Delete event didn't drop the VM from byUUID")
	}
	if l := reg.listForHost("h-remote"); len(l) != 0 {
		t.Errorf("hostIdx still references the deleted VM : %d entries", len(l))
	}
}

func TestVMRegistry_KV_ApplyKVEvent_HostUUIDFlip(t *testing.T) {
	// Simulate the cross-host claim path : another agent writes the
	// same VM record with a new host_uuid. The Put event must move
	// the entry between hostIdx slots without leaving a stale.
	kv := newMemKV("/test/vms/")
	reg, _ := loadVMRegistryKV(context.Background(), kv, nil)
	v, _ := reg.create(CreateVMSpec{
		ProjectUUID: "p-1", Name: "web", HostUUID: "dc1",
	})
	if l := reg.listForHost("dc1"); len(l) != 1 {
		t.Fatalf("dc1 should hold v ; got %d", len(l))
	}

	flipped := v
	flipped.HostUUID = "dc2"
	reg.applyKVEvent(KVEvent{
		Kind: KVPut, Key: "/test/vms/" + v.UUID, Value: encodeVMRecord(flipped),
	})
	if l := reg.listForHost("dc1"); len(l) != 0 {
		t.Errorf("dc1 still references v after host flip : %d", len(l))
	}
	if l := reg.listForHost("dc2"); len(l) != 1 {
		t.Errorf("dc2 didn't pick up the flip : %d", len(l))
	}
}

func TestVMRegistry_KV_MigrateLegacyBlobOnEmptyKV(t *testing.T) {
	// Pre-fill a blob Storage with one VM, leave the KV empty.
	blobStore := NewMemStorage()
	legacyReg, _ := loadVMRegistry(context.Background(), blobStore)
	v, _ := legacyReg.create(CreateVMSpec{
		ProjectUUID: "p-leg", Name: "legacy-vm", HostUUID: "h-leg",
		Image: "img", CPUCount: 1, MemoryMiB: 512,
	})

	kv := newMemKV("/test/vms/")
	reg, err := loadVMRegistryKV(context.Background(), kv, blobStore)
	if err != nil {
		t.Fatalf("load with migration : %v", err)
	}
	// The legacy VM should have been migrated.
	if got, ok := reg.lookupByUUID(v.UUID); !ok || got.Name != "legacy-vm" {
		t.Errorf("migration didn't populate the registry : got %+v ok=%v", got, ok)
	}
	if blob, _ := kv.GetOne(context.Background(), v.UUID); len(blob) == 0 {
		t.Error("migration didn't Put the record under the KV prefix")
	}
}

func TestVMRegistry_KV_PerformanceBenefitVsBlob(t *testing.T) {
	// Smoke : per-record save touches O(1) data, full-blob save touches
	// O(N). With 100 VMs : the KV PutOne payload should be < 1/10th
	// the blob path's payload. Quick assertion + readme-style note.
	kv := newMemKV("/test/vms/")
	reg, _ := loadVMRegistryKV(context.Background(), kv, nil)
	for i := 0; i < 100; i++ {
		_, _ = reg.create(CreateVMSpec{
			ProjectUUID: "p", Name: fmtName(i), HostUUID: "h-01",
		})
	}
	all, _ := kv.List(context.Background())
	if len(all) != 100 {
		t.Errorf("expected 100 records ; got %d", len(all))
	}
	// Each per-record blob is small. Just a sanity check.
	for _, blob := range all {
		if len(blob) > 600 {
			t.Errorf("per-record blob too large : %d bytes", len(blob))
		}
	}
}

func fmtName(i int) string {
	return string(rune('a'+(i%26))) + string(rune('0'+(i/26)%10))
}
