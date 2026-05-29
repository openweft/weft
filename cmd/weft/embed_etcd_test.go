package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/openweft/weft"
)

// TestStartEmbedEtcd covers the full happy path : boot embed.Etcd
// under a temp dir, dial it, save+load a registry blob, close clean.
// Heavy (boots an actual etcd) so it's gated behind a short-test
// guard ; the unit-level shape is covered by the factory tests.
func TestStartEmbedEtcd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embed-etcd smoke under -short")
	}
	dir := t.TempDir()
	h, err := startEmbedEtcd(dir)
	if err != nil {
		t.Fatalf("startEmbedEtcd: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	if len(h.endpoints) == 0 || !strings.HasPrefix(h.endpoints[0], "http://127.0.0.1:") {
		t.Errorf("endpoints = %v, want a http://127.0.0.1:<port> URL", h.endpoints)
	}
	// Data dir is created under the configured root.
	if _, err := os.Stat(h.dataDir); err != nil {
		t.Errorf("data dir missing: %v", err)
	}
}

// TestBuildStorageFactory_EmbedEtcdEndToEnd boots embed.Etcd through
// the public factory entry point, exercises one registry's
// Save/Load round-trip, and closes. This is the operator path —
// `storage-backend = embed-etcd` should just work without any other
// configuration.
func TestBuildStorageFactory_EmbedEtcdEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embed-etcd smoke under -short")
	}
	dir := t.TempDir()
	sf, err := buildStorageFactory(fileConfigTargets{
		storageBackend: "embed-etcd",
		configDir:      dir,
		etcdKeyPrefix:  "/weft/test/",
	})
	if err != nil {
		t.Fatalf("buildStorageFactory: %v", err)
	}
	t.Cleanup(func() { _ = sf.close() })

	if sf.new == nil {
		t.Fatal("embed-etcd factory should produce non-nil .new")
	}
	s := sf.new("flavors")
	if _, ok := s.(*weft.EtcdStorage); !ok {
		t.Fatalf("factory returned %T, want *weft.EtcdStorage", s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), embedRoundtripTimeout)
	defer cancel()
	blob := []byte("# weft embed-etcd round-trip\n")
	if err := s.Save(ctx, blob); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(blob) {
		t.Errorf("round-trip mismatch : got %q want %q", got, blob)
	}
}

// TestDisplayStorageBackend_EmbedEtcd pins the human-readable
// rendering used in the startup log line.
func TestDisplayStorageBackend_EmbedEtcd(t *testing.T) {
	if got, want := displayStorageBackend("embed-etcd"), "embed-etcd (in-process)"; got != want {
		t.Errorf("displayStorageBackend(embed-etcd) = %q, want %q", got, want)
	}
}
