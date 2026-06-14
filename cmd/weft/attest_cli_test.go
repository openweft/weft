package main

// attest_cli_test.go covers the `weft attest trust-ek` operator command's
// argument + backend guard rails. The etcd happy path is covered by the
// control-plane EK-registry tests (and exercised end-to-end by the node
// handshake against a real verifier) ; here we lock the CLI's input
// validation and the file-backend rejection so an operator gets a clean
// error instead of a confusing partial run.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runTrustEK invokes the trust-ek command with args and returns its error.
func runTrustEK(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newTrustEKCmd()
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.Execute()
}

func TestTrustEK_MissingFile(t *testing.T) {
	err := runTrustEK(t, filepath.Join(t.TempDir(), "does-not-exist.pub"))
	if err == nil || !strings.Contains(err.Error(), "read EK pub file") {
		t.Fatalf("missing file: got %v ; want a read error", err)
	}
}

func TestTrustEK_EmptyFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "empty.pub")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := runTrustEK(t, f)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty file: got %v ; want an empty-file error", err)
	}
}

// TestTrustEK_FileBackendRejected : the default (file) backend has no
// per-record KV store, so trust-ek must refuse rather than silently no-op.
func TestTrustEK_FileBackendRejected(t *testing.T) {
	f := filepath.Join(t.TempDir(), "ek.pub")
	if err := os.WriteFile(f, []byte("ek-public-area-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No --storage-backend → defaults to "file" → no newKV.
	err := runTrustEK(t, "--config-dir", t.TempDir(), f)
	if err == nil || !strings.Contains(err.Error(), "etcd storage backend") {
		t.Fatalf("file backend: got %v ; want an etcd-required error", err)
	}
}

// TestTrustEK_EtcdMissingEndpoints : --storage-backend etcd with no
// endpoints must fail at storage-open with a clear message (not panic).
func TestTrustEK_EtcdMissingEndpoints(t *testing.T) {
	f := filepath.Join(t.TempDir(), "ek.pub")
	if err := os.WriteFile(f, []byte("ek"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runTrustEK(t, "--storage-backend", "etcd", f)
	if err == nil || !strings.Contains(err.Error(), "open storage") {
		t.Fatalf("etcd no endpoints: got %v ; want an open-storage error", err)
	}
}
