package imagestore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageRaw writes a raw golden image for ref into a cache rooted at dir, using
// the same <dir>/<SanitizeRef(ref)>/raw.img layout reflinkStore resolves.
func stageRaw(t *testing.T, dir, ref string, b []byte) {
	t.Helper()
	entry := filepath.Join(dir, SanitizeRef(ref))
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatalf("mkdir cache entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entry, reflinkRawName), b, 0o644); err != nil {
		t.Fatalf("stage raw: %v", err)
	}
}

// TestReflinkStore_CloneFromCache stages a raw image under the cache layout and
// checks ImageInCache / CachedImagePath / CopyImageToDisk all agree and produce
// a byte-identical disk. On APFS/reflink this exercises the real CoW clone.
func TestReflinkStore_CloneFromCache(t *testing.T) {
	dir := t.TempDir()
	ref := "example.com/debian:13"
	want := bytes.Repeat([]byte("golden"), 2048)
	stageRaw(t, dir, ref, want)

	s := NewReflink(dir)

	if !s.ImageInCache(ref) {
		t.Fatal("ImageInCache: want true for staged image")
	}
	got, err := s.CachedImagePath(ref)
	if err != nil {
		t.Fatalf("CachedImagePath: %v", err)
	}
	if wantPath := filepath.Join(dir, SanitizeRef(ref), reflinkRawName); got != wantPath {
		t.Fatalf("CachedImagePath = %q, want %q", got, wantPath)
	}

	dst := filepath.Join(t.TempDir(), "disk.img")
	var log bytes.Buffer
	if err := s.CopyImageToDisk(ref, dst, &log); err != nil {
		t.Fatalf("CopyImageToDisk: %v", err)
	}
	gotBytes, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(gotBytes, want) {
		t.Fatalf("disk content mismatch (%d vs %d bytes)", len(gotBytes), len(want))
	}
	if !strings.Contains(log.String(), "cloning") {
		t.Fatalf("expected progress output, got %q", log.String())
	}
}

// TestReflinkStore_AbsolutePathRef covers pointing the ref straight at a golden
// image file (e.g. a raw on a ZFS dataset) instead of staging it in the cache.
func TestReflinkStore_AbsolutePathRef(t *testing.T) {
	srcDir := t.TempDir()
	golden := filepath.Join(srcDir, "debian-13.raw")
	want := []byte("absolute golden image")
	if err := os.WriteFile(golden, want, 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}

	s := NewReflink(t.TempDir()) // cache dir is irrelevant for an abs ref

	if !s.ImageInCache(golden) {
		t.Fatal("ImageInCache: want true for an absolute path to an existing image")
	}
	dst := filepath.Join(t.TempDir(), "disk.img")
	if err := s.CopyImageToDisk(golden, dst, nil); err != nil {
		t.Fatalf("CopyImageToDisk: %v", err)
	}
	gotBytes, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(gotBytes, want) {
		t.Fatalf("disk content mismatch")
	}
}

// TestReflinkStore_MissingImage covers the not-staged path across the three
// read methods.
func TestReflinkStore_MissingImage(t *testing.T) {
	s := NewReflink(t.TempDir())
	ref := "example.com/absent:1"

	if s.ImageInCache(ref) {
		t.Fatal("ImageInCache: want false for an absent image")
	}
	if _, err := s.CachedImagePath(ref); err == nil {
		t.Fatal("CachedImagePath: want error for an absent image")
	}
	if err := s.CopyImageToDisk(ref, filepath.Join(t.TempDir(), "disk.img"), nil); err == nil {
		t.Fatal("CopyImageToDisk: want error for an absent image")
	}
}

// TestReflinkStore_NotRegularFile rejects a cache entry whose raw.img is a
// directory rather than a file.
func TestReflinkStore_NotRegularFile(t *testing.T) {
	dir := t.TempDir()
	ref := "example.com/weird:1"
	if err := os.MkdirAll(filepath.Join(dir, SanitizeRef(ref), reflinkRawName), 0o755); err != nil {
		t.Fatalf("mkdir bogus raw: %v", err)
	}
	s := NewReflink(dir)

	if s.ImageInCache(ref) {
		t.Fatal("ImageInCache: want false when raw.img is a directory")
	}
	if _, err := s.CachedImagePath(ref); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("CachedImagePath: want a not-regular-file error, got %v", err)
	}
}

// TestReflinkStore_DirRoundTrip covers Dir/SetDir.
func TestReflinkStore_DirRoundTrip(t *testing.T) {
	s := NewReflink("/initial")
	if s.Dir() != "/initial" {
		t.Fatalf("Dir = %q, want /initial", s.Dir())
	}
	s.SetDir("/updated")
	if s.Dir() != "/updated" {
		t.Fatalf("Dir after SetDir = %q, want /updated", s.Dir())
	}
	s.SetChecksums(map[string]string{"a": "b"}) // no-op, must not panic
}

// TestReflinkStore_PullUnsupported documents that the pull/convert/list
// machinery is unavailable on this store.
func TestReflinkStore_PullUnsupported(t *testing.T) {
	s := NewReflink(t.TempDir())
	if err := s.PullImage(t.Context(), "example.com/x:1", nil); !errors.Is(err, errReflinkPullUnsupported) {
		t.Fatalf("PullImage err = %v, want unsupported", err)
	}
	if _, err := s.ListOCI(); !errors.Is(err, errReflinkPullUnsupported) {
		t.Fatalf("ListOCI err = %v, want unsupported", err)
	}
	if err := s.DeleteOCI("x"); !errors.Is(err, errReflinkPullUnsupported) {
		t.Fatalf("DeleteOCI err = %v, want unsupported", err)
	}
}
