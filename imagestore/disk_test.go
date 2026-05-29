//go:build darwin

package imagestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-compressions/lzfse"
	disk_qcow2 "github.com/go-diskimages/qcow2"
)

// helper: write a small qcow2 image into the supplied path for use in tests
// that exercise the qcow2 conversion branch of CopyImageToDisk.
func writeSmallQCOW2(t *testing.T, dst string) {
	t.Helper()
	// 64KiB virtual size (1 cluster) is enough to exercise ConvertToRaw
	if err := disk_qcow2.Create(dst, 64*1024); err != nil {
		t.Fatalf("qcow2.Create: %v", err)
	}
}

// TestLockFor_SameRefSameLock confirms lockFor returns the same mutex
// instance for the same ref, ensuring concurrent CopyImageToDisk calls
// per imageRef are serialised.
func TestLockFor_SameRefSameLock(t *testing.T) {
	a := lockFor("ref-X")
	b := lockFor("ref-X")
	if a != b {
		t.Errorf("lockFor returned distinct mutex instances for the same ref")
	}
}

// TestLockFor_DifferentRefsDifferentLocks confirms each ref gets its own
// mutex, so cloning unrelated images doesn't artificially serialise.
func TestLockFor_DifferentRefsDifferentLocks(t *testing.T) {
	a := lockFor("ref-A-unique-1")
	b := lockFor("ref-B-unique-1")
	if a == b {
		t.Errorf("lockFor returned the same mutex for distinct refs")
	}
}

// TestLockFor_Concurrent confirms concurrent LoadOrStore calls all see
// the same mutex (sync.Map invariant, but worth pinning).
func TestLockFor_Concurrent(t *testing.T) {
	const n = 16
	var wg sync.WaitGroup
	results := make([]*sync.Mutex, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = lockFor("shared-ref")
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Errorf("lockFor returned distinct instances under concurrency")
		}
	}
}

// TestLocateHTTPSource_RawFile confirms a raw file in a cache dir is
// returned with isQcow2=false.
func TestLocateHTTPSource_RawFile(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "myimage.raw")
	if err := os.WriteFile(rawPath, []byte("RAW-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, isQ, err := locateHTTPSource(dir)
	if err != nil {
		t.Fatalf("locateHTTPSource: %v", err)
	}
	if src != rawPath {
		t.Errorf("src = %q, want %q", src, rawPath)
	}
	if isQ {
		t.Errorf("expected isQcow2=false for a plain file")
	}
}

// TestLocateHTTPSource_QCOW2File confirms a qcow2 file is detected.
func TestLocateHTTPSource_QCOW2File(t *testing.T) {
	dir := t.TempDir()
	qcowPath := filepath.Join(dir, "myimage.qcow2")
	writeSmallQCOW2(t, qcowPath)
	src, isQ, err := locateHTTPSource(dir)
	if err != nil {
		t.Fatalf("locateHTTPSource: %v", err)
	}
	if src != qcowPath {
		t.Errorf("src = %q, want %q", src, qcowPath)
	}
	if !isQ {
		t.Errorf("expected isQcow2=true for qcow2 file")
	}
}

// TestLocateHTTPSource_SkipsRawCacheAndTmp confirms the helper ignores
// the raw.img cache entry and any *.tmp leftovers.
func TestLocateHTTPSource_SkipsRawCacheAndTmp(t *testing.T) {
	dir := t.TempDir()
	// os.ReadDir returns entries sorted lexically. rawCacheName ("raw.img")
	// and the *.tmp file must sort BEFORE the real file so the skip branch
	// is actually exercised — name the real file "zzz.raw".
	if err := os.WriteFile(filepath.Join(dir, rawCacheName), []byte("RAW"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aaa-partial.tmp"), []byte("PART"), 0o600); err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(dir, "zzz.raw")
	if err := os.WriteFile(realPath, []byte("REAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, _, err := locateHTTPSource(dir)
	if err != nil {
		t.Fatalf("locateHTTPSource: %v", err)
	}
	if src != realPath {
		t.Errorf("locator picked %q, want %q (rawCacheName + .tmp must be skipped)", src, realPath)
	}
}

// TestLocateHTTPSource_SkipsDirectories confirms the helper ignores
// nested directories (the OCI layout case shouldn't hit this branch).
func TestLocateHTTPSource_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(dir, "image.raw")
	if err := os.WriteFile(rawPath, []byte("R"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, _, err := locateHTTPSource(dir)
	if err != nil {
		t.Fatalf("locateHTTPSource: %v", err)
	}
	if src != rawPath {
		t.Errorf("expected real file %q, got %q", rawPath, src)
	}
}

// TestLocateHTTPSource_NoFile errors when the cache dir holds no files.
func TestLocateHTTPSource_NoFile(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := locateHTTPSource(dir); err == nil {
		t.Errorf("expected error when cache dir has no usable image file")
	}
}

// TestLocateHTTPSource_NonexistentDir errors when the cache dir doesn't
// exist.
func TestLocateHTTPSource_NonexistentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if _, _, err := locateHTTPSource(dir); err == nil {
		t.Errorf("expected error reading nonexistent dir")
	}
}

// TestCloneOrCopyFile_Success confirms cloneOrCopyFile clones (or copies)
// src to dst and the resulting content matches. On APFS this uses clonefile;
// elsewhere falls back to copyFileRaw.
func TestCloneOrCopyFile_Success(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	payload := []byte("clone-payload-12345")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cloneOrCopyFile(src, dst); err != nil {
		t.Fatalf("cloneOrCopyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("dst content mismatch: %q", got)
	}
}

// TestCloneOrCopyFile_OverwritesExistingDst confirms cloneOrCopyFile
// removes dst before cloning so the operation is idempotent.
func TestCloneOrCopyFile_OverwritesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	if err := os.WriteFile(src, []byte("NEW"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("PREEXISTING-CONTENT-LONGER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cloneOrCopyFile(src, dst); err != nil {
		t.Fatalf("cloneOrCopyFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "NEW" {
		t.Errorf("dst not overwritten: %q", got)
	}
}

// TestCloneOrCopyFile_MissingSrc surfaces ENOENT as an error.
func TestCloneOrCopyFile_MissingSrc(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "missing")
	dst := filepath.Join(dir, "out.bin")
	if err := cloneOrCopyFile(src, dst); err == nil {
		t.Errorf("expected error when src is missing")
	}
}

// TestCopyFileRaw_Success exercises the byte-stream fallback directly.
func TestCopyFileRaw_Success(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.bin")
	dst := filepath.Join(dir, "out.bin")
	payload := bytes.Repeat([]byte{'x'}, 8192)
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFileRaw(src, dst); err != nil {
		t.Fatalf("copyFileRaw: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("dst content mismatch (size=%d, want=%d)", len(got), len(payload))
	}
}

// TestCopyFileRaw_MissingSrc errors when src doesn't exist.
func TestCopyFileRaw_MissingSrc(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "nope")
	dst := filepath.Join(dir, "out.bin")
	err := copyFileRaw(src, dst)
	if err == nil {
		t.Errorf("expected error when src does not exist")
	}
	if !strings.Contains(err.Error(), "open source") {
		t.Errorf("error should mention 'open source', got: %v", err)
	}
}

// TestCopyFileRaw_UnwritableDst errors when the destination directory
// is unwritable.
func TestCopyFileRaw_UnwritableDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Destination directory doesn't exist
	dst := filepath.Join(dir, "no-such-dir", "out.bin")
	err := copyFileRaw(src, dst)
	if err == nil {
		t.Errorf("expected error when dst dir does not exist")
	}
	if !strings.Contains(err.Error(), "open dest") {
		t.Errorf("error should mention 'open dest', got: %v", err)
	}
}

// TestCopyFileRaw_SrcIsDirectory exercises the io.Copy error branch:
// os.Open succeeds on a directory, but reading from a directory fd
// fails (EISDIR), so io.Copy surfaces an error.
func TestCopyFileRaw_SrcIsDirectory(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src-as-dir")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.bin")
	err := copyFileRaw(srcDir, dst)
	if err == nil {
		t.Fatalf("expected error copying from a directory")
	}
	if !strings.Contains(err.Error(), "copy") {
		t.Errorf("error should mention 'copy', got: %v", err)
	}
}

// TestEnsureRawCache_FreshMaterialise confirms the function calls the
// materialise function exactly once and returns the path to the .img.
func TestEnsureRawCache_FreshMaterialise(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	got, err := ensureRawCache(dir, func(tmp string) error {
		calls++
		return os.WriteFile(tmp, []byte("RAW"), 0o600)
	})
	if err != nil {
		t.Fatalf("ensureRawCache: %v", err)
	}
	if calls != 1 {
		t.Errorf("materialise called %d times, want 1", calls)
	}
	want := filepath.Join(dir, rawCacheName)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(want); string(b) != "RAW" {
		t.Errorf("rawCache content unexpected: %q", b)
	}
}

// TestEnsureRawCache_ExistingCacheNoMaterialise: if the raw.img file
// already exists, the materialiser is not invoked.
func TestEnsureRawCache_ExistingCacheNoMaterialise(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, rawCacheName)
	if err := os.WriteFile(existing, []byte("EXISTING"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ensureRawCache(dir, func(string) error {
		t.Errorf("materialise should not be called when cache already exists")
		return nil
	})
	if err != nil {
		t.Fatalf("ensureRawCache: %v", err)
	}
	if got != existing {
		t.Errorf("got %q, want %q", got, existing)
	}
}

// TestEnsureRawCache_StaleTmpRemoved confirms a stale .tmp file from a
// previous interrupted run is cleaned up before materialise runs.
func TestEnsureRawCache_StaleTmpRemoved(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, rawCacheName+".tmp")
	if err := os.WriteFile(tmp, []byte("STALE"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ensureRawCache(dir, func(tmp string) error {
		return os.WriteFile(tmp, []byte("FRESH"), 0o600)
	})
	if err != nil {
		t.Fatalf("ensureRawCache: %v", err)
	}
	// After ensureRawCache, raw.img should contain "FRESH" + .tmp should be gone.
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file should have been renamed, still present: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, rawCacheName)); string(b) != "FRESH" {
		t.Errorf("raw.img content = %q, want FRESH", b)
	}
}

// TestEnsureRawCache_MaterialiseError propagates the materialise error
// and cleans up the .tmp file.
func TestEnsureRawCache_MaterialiseError(t *testing.T) {
	dir := t.TempDir()
	wantErr := errors.New("boom")
	_, err := ensureRawCache(dir, func(string) error { return wantErr })
	if err == nil {
		t.Fatal("expected error from materialise")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should wrap original 'boom', got: %v", err)
	}
	// .tmp must be cleaned up.
	if _, statErr := os.Stat(filepath.Join(dir, rawCacheName+".tmp")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("tmp file should have been removed after materialise failure")
	}
}

// TestEnsureRawCache_RenameError exercises the commit-rename failure
// branch by removing the parent directory before rename.
func TestEnsureRawCache_RenameError(t *testing.T) {
	dir := t.TempDir()
	_, err := ensureRawCache(dir, func(tmp string) error {
		// Write the tmp file so materialise succeeds, then delete the
		// parent dir so rename fails.
		if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
			return err
		}
		// Remove the parent directory: rename fails with ENOENT.
		return os.RemoveAll(dir)
	})
	if err == nil {
		t.Errorf("expected error when rename fails")
	}
}

// TestCachedImagePath_HappyPath: a cached raw HTTP image surfaces a
// stable path back to the caller.
func TestCachedImagePath_HappyPath(t *testing.T) {
	s := New(t.TempDir())
	ref := "https://example.com/img.raw"
	got := writeCacheFile(t, s, ref, []byte("RAW"))
	out, err := s.CachedImagePath(ref)
	if err != nil {
		t.Fatalf("CachedImagePath: %v", err)
	}
	if out != got {
		t.Errorf("CachedImagePath = %q, want %q", out, got)
	}
}

// TestCachedImagePath_OCIRefRejected: non-HTTP refs (OCI) cannot be
// patched in place and surface a clear error.
func TestCachedImagePath_OCIRefRejected(t *testing.T) {
	s := New(t.TempDir())
	_, err := s.CachedImagePath("oci://example/foo:tag")
	if err == nil {
		t.Fatal("expected error for OCI ref")
	}
	if !strings.Contains(err.Error(), "OCI") {
		t.Errorf("error should mention OCI: %v", err)
	}
}

// TestCachedImagePath_QCOW2Rejected: qcow2 images need transcoding before
// patching; CachedImagePath surfaces a dedicated error.
func TestCachedImagePath_QCOW2Rejected(t *testing.T) {
	s := New(t.TempDir())
	ref := "https://example.com/img.qcow2"
	cs := s.(*store)
	dir := cs.cacheEntryDir(ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSmallQCOW2(t, filepath.Join(dir, "img.qcow2"))
	if _, err := s.CachedImagePath(ref); err == nil {
		t.Errorf("expected error for QCOW2 image")
	} else if !strings.Contains(err.Error(), "QCOW2") {
		t.Errorf("error should mention QCOW2: %v", err)
	}
}

// TestCachedImagePath_CacheMissing errors when the cache dir doesn't
// have an image to point at.
func TestCachedImagePath_CacheMissing(t *testing.T) {
	s := New(t.TempDir())
	_, err := s.CachedImagePath("https://example.com/missing.raw")
	if err == nil {
		t.Errorf("expected error when image not in cache")
	}
}

// TestCopyImageToDisk_RawHTTPClone exercises the hot path: a cached
// raw HTTP image is cloned (or copied) directly to dst.
func TestCopyImageToDisk_RawHTTPClone(t *testing.T) {
	s := New(t.TempDir())
	ref := "https://example.com/myraw.img"
	writeCacheFile(t, s, ref, []byte("RAW-PAYLOAD"))
	dst := filepath.Join(t.TempDir(), "vm.img")
	var w bytes.Buffer
	if err := s.CopyImageToDisk(ref, dst, &w); err != nil {
		t.Fatalf("CopyImageToDisk: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "RAW-PAYLOAD" {
		t.Errorf("dst content = %q, want RAW-PAYLOAD", got)
	}
	if !strings.Contains(w.String(), "cloning") {
		t.Errorf("expected 'cloning' progress line, got: %s", w.String())
	}
}

// TestCopyImageToDisk_HTTPQCOW2Convert exercises the qcow2 → raw cache
// transcode path. Slower than the raw clone but still O(MB).
func TestCopyImageToDisk_HTTPQCOW2Convert(t *testing.T) {
	s := New(t.TempDir())
	ref := "https://example.com/myimg.qcow2"
	cs := s.(*store)
	dir := cs.cacheEntryDir(ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSmallQCOW2(t, filepath.Join(dir, "myimg.qcow2"))

	dst := filepath.Join(t.TempDir(), "vm.img")
	var w bytes.Buffer
	if err := s.CopyImageToDisk(ref, dst, &w); err != nil {
		t.Fatalf("CopyImageToDisk qcow2: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("dst not written: %v", err)
	}
	// Second call must hit the raw cache (no re-conversion).
	w.Reset()
	dst2 := filepath.Join(t.TempDir(), "vm2.img")
	if err := s.CopyImageToDisk(ref, dst2, &w); err != nil {
		t.Fatalf("CopyImageToDisk (second call) qcow2: %v", err)
	}
	if strings.Contains(w.String(), "converting") {
		t.Errorf("second call should not re-convert; output: %s", w.String())
	}
	rawCachePath := filepath.Join(dir, rawCacheName)
	if _, err := os.Stat(rawCachePath); err != nil {
		t.Errorf("raw cache should be present: %v", err)
	}
}

// TestCopyImageToDisk_HTTPRefMissing surfaces an error when the cache
// entry doesn't actually have an image.
func TestCopyImageToDisk_HTTPRefMissing(t *testing.T) {
	s := New(t.TempDir())
	ref := "https://example.com/never-cached.raw"
	cs := s.(*store)
	// Create the cache dir but no image inside.
	if err := os.MkdirAll(cs.cacheEntryDir(ref), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyImageToDisk(ref, filepath.Join(t.TempDir(), "dst"), io.Discard); err == nil {
		t.Errorf("expected error when cache is empty")
	}
}

// TestCopyImageToDisk_OCIRef_FailsWithoutLayout exercises the OCI path
// without a real layout : ExtractDisk fails because index.json is absent.
func TestCopyImageToDisk_OCIRef_FailsWithoutLayout(t *testing.T) {
	s := New(t.TempDir())
	ref := "ghcr.io/foo/bar:latest"
	// Create an empty cache entry dir for the OCI ref so the lookup gets
	// past os.MkdirAll but ExtractDisk fails on missing index.json.
	cs := s.(*store)
	if err := os.MkdirAll(cs.cacheEntryDir(ref), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyImageToDisk(ref, filepath.Join(t.TempDir(), "dst.img"), io.Discard); err == nil {
		t.Errorf("expected error when OCI layout is missing")
	}
}

// writeTartOCILayout builds a minimal tart-compatible OCI layout under
// dir with a single tart.disk.v2 layer (LZFSE-compressed raw payload).
func writeTartOCILayout(t *testing.T, dir string, rawPayload []byte) {
	t.Helper()
	const diskMediaType = "application/vnd.cirruslabs.tart.disk.v2"

	compressed, err := lzfse.Compress(rawPayload)
	if err != nil {
		t.Fatalf("lzfse.Compress: %v", err)
	}
	blobsDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Layer blob.
	layerDigest := sha256Hex(compressed)
	if err := os.WriteFile(filepath.Join(blobsDir, layerDigest), compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	// Manifest referencing the layer with the required uncompressed-size
	// annotation.
	manifest := map[string]interface{}{
		"layers": []map[string]interface{}{
			{
				"mediaType": diskMediaType,
				"digest":    "sha256:" + layerDigest,
				"size":      len(compressed),
				"annotations": map[string]string{
					"org.cirruslabs.tart.uncompressed-size": strconv.Itoa(len(rawPayload)),
				},
			},
		},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDigest := sha256Hex(manifestBytes)
	if err := os.WriteFile(filepath.Join(blobsDir, manifestDigest), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// index.json pointing at the manifest.
	index := map[string]interface{}{
		"manifests": []map[string]interface{}{
			{"digest": "sha256:" + manifestDigest, "mediaType": "application/vnd.oci.image.manifest.v1+json"},
		},
	}
	indexBytes, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), indexBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCopyImageToDisk_OCIRef_ExtractAndClone exercises the OCI happy
// path: a valid tart OCI layout is extracted into a raw cache then
// cloned to the destination. Covers the OCI branch of CopyImageToDisk +
// the post-extract clone line.
func TestCopyImageToDisk_OCIRef_ExtractAndClone(t *testing.T) {
	s := New(t.TempDir())
	ref := "ghcr.io/cirruslabs/example:latest"
	cs := s.(*store)
	cacheDir := cs.cacheEntryDir(ref)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rawPayload := bytes.Repeat([]byte("RAW-DISK-CONTENT"), 256) // 4KiB
	writeTartOCILayout(t, cacheDir, rawPayload)

	dst := filepath.Join(t.TempDir(), "vm.img")
	var w bytes.Buffer
	if err := s.CopyImageToDisk(ref, dst, &w); err != nil {
		t.Fatalf("CopyImageToDisk OCI: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, rawPayload) {
		t.Errorf("dst content mismatch (size=%d, want=%d)", len(got), len(rawPayload))
	}
	if !strings.Contains(w.String(), "extracting OCI") {
		t.Errorf("expected 'extracting OCI' message, got: %s", w.String())
	}
	// Second call must hit the raw cache (no re-extract).
	w.Reset()
	dst2 := filepath.Join(t.TempDir(), "vm2.img")
	if err := s.CopyImageToDisk(ref, dst2, &w); err != nil {
		t.Fatalf("CopyImageToDisk OCI (second): %v", err)
	}
	if strings.Contains(w.String(), "extracting OCI") {
		t.Errorf("second call re-extracted; output: %s", w.String())
	}
}
