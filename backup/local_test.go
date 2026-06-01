package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFile is a tiny test helper.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLocalBackend_Roundtrip exercises Upload → List → Download
// → Delete on a filesystem-rooted backend.
func TestLocalBackend_Roundtrip(t *testing.T) {
	root := t.TempDir()
	be, err := NewLocalBackend(root)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	// Stage a "snapshot blob" in a separate src dir.
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "snap.bin")
	payload := []byte("hello-snapshot-blob")
	writeFile(t, srcPath, payload)

	ctx := context.Background()
	key := SnapshotKey("proj-1", "vol-1", "snap-1")
	if key == "" {
		t.Fatalf("SnapshotKey returned empty")
	}

	if err := be.Upload(ctx, srcPath, key); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// List with the project prefix should yield exactly one entry.
	entries, err := be.List(ctx, "proj-1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != key {
		t.Errorf("List entries = %+v, want one with Key=%q", entries, key)
	}
	if entries[0].Size != int64(len(payload)) {
		t.Errorf("entry size = %d, want %d", entries[0].Size, len(payload))
	}

	// Download into a fresh path and confirm bytes match.
	dst := filepath.Join(t.TempDir(), "restored.bin")
	if err := be.Download(ctx, key, dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round-trip payload mismatch: got %q want %q", got, payload)
	}

	// Delete and confirm subsequent Download returns ErrNotFound.
	if err := be.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	err = be.Download(ctx, key, filepath.Join(t.TempDir(), "after-delete.bin"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Download after Delete: err = %v, want ErrNotFound", err)
	}

	// Idempotent delete: second call must not error.
	if err := be.Delete(ctx, key); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// TestLocalBackend_List_EmptyRoot covers the "no keys yet" branch
// — a brand-new bucket should yield zero entries, not an error.
func TestLocalBackend_List_EmptyRoot(t *testing.T) {
	root := t.TempDir()
	be, err := NewLocalBackend(root)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	entries, err := be.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected zero entries on empty root, got %d : %+v", len(entries), entries)
	}
}

// TestLocalBackend_RejectsTraversalKey is the security guardrail
// for ill-formed operator inputs : a key with ".." segments that
// escapes the root must be rejected before any filesystem I/O.
func TestLocalBackend_RejectsTraversalKey(t *testing.T) {
	root := t.TempDir()
	be, _ := NewLocalBackend(root)
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	writeFile(t, srcPath, []byte("x"))

	cases := []string{
		"../escaped.qcow2",
		"../../etc/passwd",
		"/abs/key.qcow2",
	}
	for _, k := range cases {
		err := be.Upload(context.Background(), srcPath, k)
		if err == nil {
			t.Errorf("Upload(%q) should have rejected traversal, got nil error", k)
		}
	}
}

// TestLocalBackend_MissingKey_Download asserts ErrNotFound is
// returned (and is errors.Is-compatible) for a key that was never
// uploaded.
func TestLocalBackend_MissingKey_Download(t *testing.T) {
	root := t.TempDir()
	be, _ := NewLocalBackend(root)
	dst := filepath.Join(t.TempDir(), "out.bin")
	err := be.Download(context.Background(), "proj/vol/no-such.qcow2", dst)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Download missing key: err = %v, want ErrNotFound", err)
	}
}

// TestLocalBackend_EmptyConstructor_Reject keeps NewLocalBackend's
// pre-condition asserted.
func TestLocalBackend_EmptyConstructor_Reject(t *testing.T) {
	if _, err := NewLocalBackend(""); err == nil {
		t.Errorf("NewLocalBackend(\"\") returned nil error")
	}
}

// TestLocalBackend_Upload_Overwrite ensures Upload over an
// existing key replaces the blob atomically (rename-on-top).
func TestLocalBackend_Upload_Overwrite(t *testing.T) {
	root := t.TempDir()
	be, _ := NewLocalBackend(root)

	srcDir := t.TempDir()
	src1 := filepath.Join(srcDir, "v1.bin")
	src2 := filepath.Join(srcDir, "v2.bin")
	writeFile(t, src1, []byte("first"))
	writeFile(t, src2, []byte("second-and-longer"))

	ctx := context.Background()
	key := "p/v/s.qcow2"
	if err := be.Upload(ctx, src1, key); err != nil {
		t.Fatalf("Upload v1: %v", err)
	}
	if err := be.Upload(ctx, src2, key); err != nil {
		t.Fatalf("Upload v2: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out.bin")
	if err := be.Download(ctx, key, dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "second-and-longer" {
		t.Errorf("after overwrite, payload = %q, want %q", got, "second-and-longer")
	}
}
