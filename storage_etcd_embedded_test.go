package weft

// storage_etcd_embedded_test.go runs EtcdStorage end-to-end against
// an embedded etcd v3 server spun up inside the test process. This
// is what proves the Load/Save path actually crosses gRPC + the
// etcd KV engine without requiring an out-of-process etcd binary
// on the test host.
//
// embed.StartEtcd lays down a small etcd server backed by a
// temporary boltdb file. ReadyNotify blocks until the new server
// is leader. The fixture closes both ends on test cleanup.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

// startEmbeddedEtcd brings up a single-node etcd, returns the
// client URL the test can dial. t.Cleanup tears the server down.
func startEmbeddedEtcd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := embed.NewConfig()
	cfg.Dir = filepath.Join(dir, "etcd-data")
	// Bind to a free loopback port so multiple parallel tests don't
	// collide. embed.NewConfig defaults to :2379 which would conflict.
	listenURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", freePort(t)))
	if err != nil {
		t.Fatalf("parse listen url: %v", err)
	}
	cfg.ListenClientUrls = []url.URL{*listenURL}
	cfg.AdvertiseClientUrls = []url.URL{*listenURL}
	peerURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", freePort(t)))
	if err != nil {
		t.Fatalf("parse peer url: %v", err)
	}
	cfg.ListenPeerUrls = []url.URL{*peerURL}
	cfg.AdvertisePeerUrls = []url.URL{*peerURL}
	cfg.InitialCluster = cfg.Name + "=" + peerURL.String()
	cfg.LogLevel = "error" // keep test output clean
	e, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatalf("start embedded etcd: %v", err)
	}
	select {
	case <-e.Server.ReadyNotify():
	case <-time.After(20 * time.Second):
		e.Server.Stop()
		t.Fatal("embedded etcd never became leader within 20s")
	}
	t.Cleanup(func() { e.Close() })
	return listenURL.String()
}

// freePort opens a tcp listener on :0 to grab a free port, then
// closes it and returns the port. There's a tiny TOCTOU window
// where another process could grab the port; in practice the test
// runner cycles fast enough that it's not an issue.
func freePort(t *testing.T) int {
	t.Helper()
	// Use ":0" — the kernel picks. We have a small dance via net to
	// extract it. Keeping this import-light by using net via embed
	// is awkward, so accept the import.
	l, err := listenTCPZero()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.close()
	return l.port()
}

// listenTCPZero is broken out so the freePort import surface stays
// narrow — we don't want net.Listen all over the test file.
type freePortListener interface {
	close() error
	port() int
}

func listenTCPZero() (freePortListener, error) {
	return openZeroListener()
}

// TestEtcdStorage_EmbeddedRoundTrip exercises the full Load/Save
// path against a real etcd KV engine.
func TestEtcdStorage_EmbeddedRoundTrip(t *testing.T) {
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

	s := NewEtcdStorageWithClient(cli, "/vzd/test/", "projects")
	if got := s.Key(); got != "/vzd/test/projects" {
		t.Errorf("Key = %q, want /vzd/test/projects", got)
	}

	// Fresh key → Load returns (nil, nil) — same as FileStorage on
	// a missing file.
	blob, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("initial Load: %v", err)
	}
	if blob != nil {
		t.Errorf("initial Load on empty key returned %q, want nil", blob)
	}

	// Save → Load round-trip.
	payload := []byte("project \"abc\" { name = \"demo\" }\n")
	if err := s.Save(ctx, payload); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("Load mismatch: got %q, want %q", got, payload)
	}

	// Overwrite via Save — linearizable Put semantics.
	payload2 := []byte("project \"abc\" { name = \"renamed\" }\n")
	if err := s.Save(ctx, payload2); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	got2, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("post-overwrite Load: %v", err)
	}
	if string(got2) != string(payload2) {
		t.Errorf("post-overwrite Load mismatch: got %q, want %q", got2, payload2)
	}

	// Independent registry on the same client gets its own key.
	s2 := NewEtcdStorageWithClient(cli, "/vzd/test/", "users")
	if got, _ := s2.Load(ctx); got != nil {
		t.Errorf("users Load should be nil (different key), got %q", got)
	}

	// Close on a non-owned storage doesn't kill the client.
	if err := s.Close(); err != nil {
		t.Errorf("Close on non-owned EtcdStorage: %v", err)
	}
	// Confirm cli is still usable.
	if _, err := s2.Load(ctx); err != nil {
		t.Errorf("post-Close client should still be usable: %v", err)
	}
}

// TestProjectRegistry_RoundTripViaEmbeddedEtcd proves the
// projectRegistry HCL encode/decode path round-trips correctly
// through an etcd backend, not just MemStorage.
func TestProjectRegistry_RoundTripViaEmbeddedEtcd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short: brings up an embedded etcd")
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

	storage := NewEtcdStorageWithClient(cli, "/vzd/test/", "projects")

	// First load — empty registry.
	reg, err := loadProjectRegistry(ctx, storage)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(reg.byUUID) != 0 {
		t.Fatalf("fresh registry should be empty, has %d entries", len(reg.byUUID))
	}

	// Create two projects.
	p1, _, err := reg.getOrCreate("team-alpha")
	if err != nil {
		t.Fatalf("getOrCreate alpha: %v", err)
	}
	p2, _, err := reg.getOrCreate("team-beta")
	if err != nil {
		t.Fatalf("getOrCreate beta: %v", err)
	}

	// Re-load from etcd via a fresh registry instance — verifies the
	// HCL blob landed there and decodes cleanly.
	reg2, err := loadProjectRegistry(ctx, storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(p1.UUID)
	if !ok || got.Name != "team-alpha" {
		t.Errorf("reload missed team-alpha: got %+v ok=%v", got, ok)
	}
	got, ok = reg2.lookupByUUID(p2.UUID)
	if !ok || got.Name != "team-beta" {
		t.Errorf("reload missed team-beta: got %+v ok=%v", got, ok)
	}
}

// Sanity check: errors.Is against ErrEtcdNotWired still resolves
// (the legacy sentinel is retained for backward compat per the
// storage.go comment).
func TestErrEtcdNotWired_StillExported(t *testing.T) {
	if !errors.Is(ErrEtcdNotWired, ErrEtcdNotWired) {
		t.Fatal("ErrEtcdNotWired is somehow not equal to itself")
	}
}
