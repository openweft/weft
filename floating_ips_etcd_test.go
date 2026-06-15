package weft

// floating_ips_etcd_test.go proves the floatingIPRegistry round-trips
// cleanly through EtcdStorage — same coverage the projectRegistry
// gets in storage_etcd_embedded_test.go. The point is to confirm
// the HCL encode/decode path (RFC3339Nano timestamps, optional
// mapped_to / target_kind, enum strings) survives the blob trip
// across etcd, not just MemStorage.
//
// The blob shape is intentional : floating-IPs sit at the same
// cardinality as networks / security_groups / ports (tens to
// hundreds per cluster) which all stay blob-shaped. The
// per-record KV path (KVStorage) is reserved for high-cardinality
// inventories (vms / hosts / projects / scheduling-rules) where
// every mutation touching the full blob would be wasteful.

import (
	"context"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestFloatingIPRegistry_RoundTripViaEmbeddedEtcd brings up the
// in-process etcd fixture, persists two FIPs (one available, one
// mapped to a VM target), then re-loads from etcd via a fresh
// registry instance and checks every field made it through.
func TestFloatingIPRegistry_RoundTripViaEmbeddedEtcd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short: brings up an embedded etcd (~1-2s startup)")
	}
	clientURL := startEmbeddedEtcd(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{clientURL},
		DialTimeout: 5 * time.Second,
		Context:     ctx,
	})
	if err != nil {
		t.Fatalf("clientv3.New: %v", err)
	}
	defer cli.Close()

	storage := NewEtcdStorageWithClient(cli, "/weft/test/", "floating_ips")
	if got := storage.Key(); got != "/weft/test/floating_ips" {
		t.Errorf("Key = %q, want /weft/test/floating_ips", got)
	}

	// Fresh load — empty registry, same contract FileStorage offers
	// on a missing file.
	reg, err := loadFloatingIPRegistry(ctx, storage)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(reg.byUUID) != 0 {
		t.Fatalf("fresh registry should be empty, has %d entries", len(reg.byUUID))
	}

	// Allocate two addresses on the same network — one stays
	// available, one gets mapped to a VM target. Exercises both
	// the "no mapped_to" and "mapped_to + target_kind set" branches
	// of the HCL writer.
	fip1, err := reg.allocate(AllocateFloatingIPSpec{
		ProjectUUID: "p1",
		NetworkUUID: "edge-net",
	}, "203.0.113.0/29")
	if err != nil {
		t.Fatalf("allocate fip1: %v", err)
	}
	fip2, err := reg.allocate(AllocateFloatingIPSpec{
		ProjectUUID: "p1",
		NetworkUUID: "edge-net",
	}, "203.0.113.0/29")
	if err != nil {
		t.Fatalf("allocate fip2: %v", err)
	}
	if _, err := reg.mapTo(fip2.UUID, FIPTargetVM, "vm-web-1", 0); err != nil {
		t.Fatalf("mapTo fip2: %v", err)
	}

	// Re-load from etcd via a fresh registry instance — the blob
	// has to have landed and decode cleanly. We check every field
	// individually so an HCL schema regression (e.g. someone
	// dropping the `,optional` on mapped_to) fails loudly here.
	reg2, err := loadFloatingIPRegistry(ctx, storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(reg2.byUUID); got != 2 {
		t.Fatalf("reload should yield 2 entries, got %d", got)
	}

	got1, ok := reg2.lookupByUUID(fip1.UUID)
	if !ok {
		t.Fatalf("fip1 missing after reload")
	}
	if got1.Address != fip1.Address {
		t.Errorf("fip1 Address: got %q want %q", got1.Address, fip1.Address)
	}
	if got1.ProjectUUID != "p1" || got1.NetworkUUID != "edge-net" {
		t.Errorf("fip1 project/network mismatch: %+v", got1)
	}
	if got1.Status != FIPStatusAvailable {
		t.Errorf("fip1 Status: got %q want %q", got1.Status, FIPStatusAvailable)
	}
	if got1.MappedTo != "" || got1.TargetKind != "" {
		t.Errorf("fip1 should be unmapped, got mapped_to=%q kind=%q", got1.MappedTo, got1.TargetKind)
	}
	// RFC3339Nano round-trip — Equal() because Parse may pick a
	// different *Location pointer than time.Now().UTC().
	if !got1.AllocatedAt.Equal(fip1.AllocatedAt) {
		t.Errorf("fip1 AllocatedAt drift: got %v want %v", got1.AllocatedAt, fip1.AllocatedAt)
	}

	got2, ok := reg2.lookupByUUID(fip2.UUID)
	if !ok {
		t.Fatalf("fip2 missing after reload")
	}
	if got2.MappedTo != "vm-web-1" {
		t.Errorf("fip2 MappedTo: got %q want vm-web-1", got2.MappedTo)
	}
	if got2.TargetKind != FIPTargetVM {
		t.Errorf("fip2 TargetKind: got %q want vm", got2.TargetKind)
	}
	if got2.Status != FIPStatusActive {
		t.Errorf("fip2 Status: got %q want active", got2.Status)
	}

	// Secondary indices have to be rebuilt from the reloaded blob —
	// the reconciler's listForTarget query depends on it.
	mapped := reg2.listForTarget(FIPTargetVM, "vm-web-1")
	if len(mapped) != 1 || mapped[0].UUID != fip2.UUID {
		t.Errorf("listForTarget after reload: got %+v, want [fip2]", mapped)
	}
	perProject := reg2.listForProject("p1")
	if len(perProject) != 2 {
		t.Errorf("listForProject after reload: got %d, want 2", len(perProject))
	}

	// Mutation on the reloaded registry persists via the same
	// EtcdStorage — releases fip1, third load should observe one
	// entry, the still-mapped fip2.
	if _, err := reg2.unmap(fip2.UUID); err != nil {
		t.Fatalf("unmap fip2: %v", err)
	}
	if _, err := reg2.release(fip2.UUID); err != nil {
		t.Fatalf("release fip2: %v", err)
	}
	if _, err := reg2.release(fip1.UUID); err != nil {
		t.Fatalf("release fip1: %v", err)
	}
	reg3, err := loadFloatingIPRegistry(ctx, storage)
	if err != nil {
		t.Fatalf("third load: %v", err)
	}
	if len(reg3.byUUID) != 0 {
		t.Errorf("post-release registry should be empty, has %d entries", len(reg3.byUUID))
	}
}
