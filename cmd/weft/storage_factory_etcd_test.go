package main

import (
	"testing"

	"github.com/openweft/weft"
)

// TestBuildStorageFactory_EtcdLazyClient covers the etcd success path of
// buildStorageFactory without a live cluster. clientv3.New does not dial
// eagerly (no grpc.WithBlock), so it returns a usable *clientv3.Client whose
// connection is established lazily on first RPC. That lets us exercise the
// factory-construction branch (lines 58-77) — the closure, the EtcdStorage
// binding, and the close hook — entirely offline.
func TestBuildStorageFactory_EtcdLazyClient(t *testing.T) {
	sf, err := buildStorageFactory(fileConfigTargets{
		storageBackend: "etcd",
		etcdEndpoints:  []string{"http://127.0.0.1:2379"},
		etcdKeyPrefix:  "/weft/test/",
	})
	if err != nil {
		t.Fatalf("buildStorageFactory(etcd): %v", err)
	}
	if sf == nil || sf.new == nil {
		t.Fatal("etcd factory and its constructor must be non-nil")
	}
	t.Cleanup(func() { _ = sf.close() })

	s := sf.new("projects")
	if _, ok := s.(*weft.EtcdStorage); !ok {
		t.Fatalf("factory returned %T, want *weft.EtcdStorage", s)
	}
}
