package weft

import (
	"context"
	"testing"
	"time"
)

// TestNewEtcdStorage_ConfigConstructor covers the config-based
// NewEtcdStorage path (endpoints → own client) against the embedded etcd,
// distinct from NewEtcdStorageWithClient. It exercises the dial, a Save/Load
// round-trip, and Close (which must tear down the owned client connection).
func TestNewEtcdStorage_ConfigConstructor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short: brings up an embedded etcd")
	}
	clientURL := startEmbeddedEtcd(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := NewEtcdStorage(ctx, EtcdConfig{
		Endpoints: []string{clientURL},
		KeyPrefix: "/vzd/cfgtest/",
	}, "projects")
	if err != nil {
		t.Fatalf("NewEtcdStorage: %v", err)
	}

	payload := []byte("project \"x\" { name = \"demo\" }\n")
	if err := s.Save(ctx, payload); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("Load = %q, want %q", got, payload)
	}

	// owned == true → Close tears down the client connection.
	if err := s.Close(); err != nil {
		t.Errorf("Close (owned): %v", err)
	}
}
