//go:build darwin

package imagestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// -- PullImage HTTP --------------------------------------------------------

// TestPullImage_HTTPS_FreshDownload uses a TLS server (https://) so the
// pull path under test (`isHTTPRef → pullHTTP`) is exercised. The store
// uses http.DefaultClient → the test installs an InsecureSkipVerify
// transport for the duration of the test.
func TestPullImage_HTTPS_FreshDownload(t *testing.T) {
	payload := []byte("hello world image bytes 1234567890")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	// Swap http.DefaultClient's transport for one that trusts the test
	// server. Restore on test exit.
	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	http.DefaultClient.Transport = srv.Client().Transport

	s := New(t.TempDir())
	ref := srv.URL + "/myimage.raw"
	var w bytes.Buffer
	if err := s.PullImage(context.Background(), ref, &w); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	cs := s.(*store)
	out := filepath.Join(cs.cacheEntryDir(ref), "myimage.raw")
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read cached file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("cached payload mismatch")
	}
	if !strings.Contains(w.String(), "pulled") {
		t.Errorf("expected 'pulled' line, got: %s", w.String())
	}
}

// TestPullImage_HTTPS_ServerError surfaces a non-200 response as a
// readable error.
func TestPullImage_HTTPS_ServerError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	http.DefaultClient.Transport = srv.Client().Transport

	s := New(t.TempDir())
	ref := srv.URL + "/error.raw"
	err := s.PullImage(context.Background(), ref, io.Discard)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention HTTP 500: %v", err)
	}
}

// TestPullImage_HTTPS_AlreadyCached_NoChecksum returns immediately when
// the image is already in the cache and no checksum is configured.
func TestPullImage_HTTPS_AlreadyCached_NoChecksum(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be called when cache hit + no checksum")
	}))
	defer srv.Close()
	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	http.DefaultClient.Transport = srv.Client().Transport

	s := New(t.TempDir())
	ref := srv.URL + "/cached.raw"
	writeCacheFile(t, s, ref, []byte("preloaded"))

	var w bytes.Buffer
	if err := s.PullImage(context.Background(), ref, &w); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if !strings.Contains(w.String(), "already cached") {
		t.Errorf("expected 'already cached' message: %s", w.String())
	}
}

// TestPullImage_HTTPS_CacheRevalidated_OK confirms the checksum-validated
// hit path : when the cached image's checksum matches the configured
// checksum URL, the pull short-circuits without re-downloading.
func TestPullImage_HTTPS_CacheRevalidated_OK(t *testing.T) {
	payload := []byte("cached-content-matching-checksum")
	h := sha256.Sum256(payload)
	checksum := hex.EncodeToString(h[:])

	var fetchCount int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This server only serves CHECKSUM requests in this test path.
		if strings.HasSuffix(r.URL.Path, "CHECKSUM") {
			fmt.Fprintf(w, "SHA256 (cached.raw) = %s\n", checksum)
			return
		}
		fetchCount++
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	http.DefaultClient.Transport = srv.Client().Transport

	s := New(t.TempDir())
	ref := srv.URL + "/cached.raw"
	csURL := srv.URL + "/CHECKSUM"
	writeCacheFile(t, s, ref, payload)
	s.SetChecksums(map[string]string{ref: csURL})

	var w bytes.Buffer
	if err := s.PullImage(context.Background(), ref, &w); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if fetchCount != 0 {
		t.Errorf("expected no image re-download, got %d", fetchCount)
	}
	if !strings.Contains(w.String(), "already cached") {
		t.Errorf("expected 'already cached', got: %s", w.String())
	}
}

// TestPullImage_HTTPS_CacheRevalidated_Mismatch_Redownloads confirms a
// failed checksum forces a re-download.
func TestPullImage_HTTPS_CacheRevalidated_Mismatch_Redownloads(t *testing.T) {
	newPayload := []byte("fresh-payload-from-server")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "CHECKSUM") {
			fmt.Fprintf(w, "SHA256 (img.raw) = %064x\n", 0)
			return
		}
		_, _ = w.Write(newPayload)
	}))
	defer srv.Close()
	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	http.DefaultClient.Transport = srv.Client().Transport

	s := New(t.TempDir())
	ref := srv.URL + "/img.raw"
	csURL := srv.URL + "/CHECKSUM"
	writeCacheFile(t, s, ref, []byte("STALE-CONTENT-DIFFERENT"))
	s.SetChecksums(map[string]string{ref: csURL})

	var w bytes.Buffer
	if err := s.PullImage(context.Background(), ref, &w); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if !strings.Contains(w.String(), "re-downloading") {
		t.Errorf("expected 're-downloading' on checksum mismatch: %s", w.String())
	}
	cs := s.(*store)
	got, _ := os.ReadFile(filepath.Join(cs.cacheEntryDir(ref), "img.raw"))
	if !bytes.Equal(got, newPayload) {
		t.Errorf("cache not refreshed after mismatch")
	}
}

// TestPullImage_HTTPS_BadURL surfaces an error on a malformed URL via
// http.NewRequestWithContext.
func TestPullImage_HTTPS_BadURL(t *testing.T) {
	s := New(t.TempDir())
	// Bad URL: still https:// prefix so the HTTP path is taken.
	ref := "https://%xx-not-a-valid-host/foo.raw"
	err := s.PullImage(context.Background(), ref, io.Discard)
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

// TestPullImage_HTTPS_NetworkFailure surfaces transport errors. Use a
// server that we close before the request to guarantee a dial error.
func TestPullImage_HTTPS_NetworkFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // ensure no connection possible

	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	// Use a transport that won't reach the closed server (no TLS trust here)
	// — connection refused fires before TLS even matters.
	http.DefaultClient.Transport = http.DefaultTransport

	s := New(t.TempDir())
	ref := url + "/img.raw"
	err := s.PullImage(context.Background(), ref, io.Discard)
	if err == nil {
		t.Fatal("expected network failure error")
	}
}

// TestPullImage_HTTPS_NoContentLength exercises the streaming path where
// the server doesn't advertise a content length, so progressReader is
// bypassed and Copy uses the raw body.
func TestPullImage_HTTPS_NoContentLength(t *testing.T) {
	payload := []byte("chunked-or-unknown-size")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Don't set Content-Length explicitly.
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	http.DefaultClient.Transport = srv.Client().Transport

	s := New(t.TempDir())
	ref := srv.URL + "/x.raw"
	if err := s.PullImage(context.Background(), ref, io.Discard); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
}

// -- ListOCI / DeleteOCI / ImageInCache ------------------------------------

// TestListOCI_Empty: ListOCI on an empty cache directory returns nil
// with no error.
func TestListOCI_Empty(t *testing.T) {
	s := New(t.TempDir())
	got, err := s.ListOCI()
	if err != nil {
		t.Fatalf("ListOCI: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %v", got)
	}
}

// TestListOCI_NonexistentDir: when the cache directory doesn't exist,
// ListOCI returns (nil, nil) instead of an error.
func TestListOCI_NonexistentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	s := New(dir)
	got, err := s.ListOCI()
	if err != nil {
		t.Errorf("ListOCI should not error on missing dir: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil list, got %v", got)
	}
}

// TestListOCI_ReadDirError surfaces a non-IsNotExist error: the store
// directory is a regular file, so os.ReadDir returns ENOTDIR.
func TestListOCI_ReadDirError(t *testing.T) {
	tmp := t.TempDir()
	asFile := filepath.Join(tmp, "store-as-file")
	if err := os.WriteFile(asFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(asFile)
	_, err := s.ListOCI()
	if err == nil {
		t.Errorf("expected error when store dir is a regular file")
	}
}

// TestListOCI_WithEntries returns each cached ref as a property map
// with `name` and `source` keys.
func TestListOCI_WithEntries(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	// Create some sanitized entries.
	for _, name := range []string{
		"ghcr.io_org_image__tag",
		"registry.example.com_repo_other__v1.0",
	} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Plain file at the root should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListOCI()
	if err != nil {
		t.Fatalf("ListOCI: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d : %v", len(items), items)
	}
	for _, it := range items {
		if it["name"] != it["source"] {
			t.Errorf("name should equal source: %v", it)
		}
		if !strings.Contains(fmt.Sprintf("%v", it["name"]), "/") && !strings.Contains(fmt.Sprintf("%v", it["name"]), ":") {
			// At least one of these should be present after unsanitize
			t.Errorf("expected unsanitized ref to contain / or :, got %v", it["name"])
		}
	}
}

// TestDeleteOCI_RemovesEntry: the named cache directory is removed.
func TestDeleteOCI_RemovesEntry(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ref := "ghcr.io/foo/bar:latest"
	cs := s.(*store)
	entryDir := cs.cacheEntryDir(ref)
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entryDir, "index.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOCI(ref); err != nil {
		t.Fatalf("DeleteOCI: %v", err)
	}
	if _, err := os.Stat(entryDir); !os.IsNotExist(err) {
		t.Errorf("expected entry dir to be gone, stat err=%v", err)
	}
}

// TestDeleteOCI_MissingEntry: removing a non-existent entry is a no-op.
func TestDeleteOCI_MissingEntry(t *testing.T) {
	s := New(t.TempDir())
	if err := s.DeleteOCI("not-cached-anywhere"); err != nil {
		t.Errorf("DeleteOCI on missing entry should be a no-op, got %v", err)
	}
}

// TestImageInCache_OCIPresent confirms an OCI cache entry is detected
// via the presence of an index.json file.
func TestImageInCache_OCIPresent(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ref := "ghcr.io/foo/bar:tag"
	cs := s.(*store)
	entryDir := cs.cacheEntryDir(ref)
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entryDir, "index.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.ImageInCache(ref) {
		t.Errorf("expected ImageInCache to detect OCI cache via index.json")
	}
}

// TestImageInCache_OCIAbsent: a directory with no index.json doesn't
// count.
func TestImageInCache_OCIAbsent(t *testing.T) {
	s := New(t.TempDir())
	cs := s.(*store)
	ref := "ghcr.io/foo/bar:tag"
	if err := os.MkdirAll(cs.cacheEntryDir(ref), 0o700); err != nil {
		t.Fatal(err)
	}
	if s.ImageInCache(ref) {
		t.Errorf("expected ImageInCache to return false without index.json")
	}
}

// TestImageInCache_HTTPDirOnly: a cache dir with only subdirectories
// (no files) is treated as not-in-cache.
func TestImageInCache_HTTPDirOnly(t *testing.T) {
	s := New(t.TempDir())
	cs := s.(*store)
	ref := "https://example.com/x.raw"
	dir := cs.cacheEntryDir(ref)
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if s.ImageInCache(ref) {
		t.Errorf("expected ImageInCache to return false when only subdirs are present")
	}
}

// -- estimateOCITotalBytes / pullOCI ---------------------------------------

// TestSumManifestBytes_ImageIndex_NoArchMatch exercises the
// image-index branch of sumManifestBytes : the loop returns 0 when no
// manifest entry matches runtime.GOARCH. Uses a synthetic index with
// an obviously unmatched architecture so neither branch fetches.
func TestSumManifestBytes_ImageIndex_NoArchMatch(t *testing.T) {
	idx := ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    digest.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111"),
				Size:      100,
				Platform:  &ocispec.Platform{Architecture: "no-such-arch", OS: "linux"},
			},
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    digest.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222"),
				Size:      100,
				Platform:  nil, // also skipped
			},
		},
	}
	idxData, _ := json.Marshal(idx)
	got := sumManifestBytes(context.Background(), nil, ocispec.MediaTypeImageIndex, idxData)
	if got != 0 {
		t.Errorf("expected 0 when no arch matches, got %d", got)
	}
}


// TestSumManifestBytes_ImageIndex_Malformed exercises the JSON-error
// branch of the index switch arm.
func TestSumManifestBytes_ImageIndex_Malformed(t *testing.T) {
	if got := sumManifestBytes(context.Background(), nil, ocispec.MediaTypeImageIndex, []byte("not-json")); got != 0 {
		t.Errorf("expected 0 for malformed index JSON, got %d", got)
	}
}

// TestPullImage_OCI_FailsOnUnreachableRegistry exercises pullOCI's
// failure when the registry is unreachable. Run with a non-routable
// "oci://" ref - we expect a clean error rather than a panic.
func TestPullImage_OCI_FailsOnUnreachableRegistry(t *testing.T) {
	s := New(t.TempDir())
	// 127.0.0.1:1 is reserved by IANA — connection refused.
	ref := "127.0.0.1:1/missing/never:latest"
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the dial doesn't wait forever
	err := s.PullImage(ctx, ref, io.Discard)
	if err == nil {
		t.Errorf("expected error for unreachable registry")
	}
}

// TestPullImage_HTTPS_OpenFileError exercises the os.OpenFile failure
// branch in pullHTTP: the destination filename already exists as a
// directory, so creating the file fails.
func TestPullImage_HTTPS_OpenFileError(t *testing.T) {
	payload := []byte("data")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	http.DefaultClient.Transport = srv.Client().Transport

	s := New(t.TempDir())
	ref := srv.URL + "/collide.raw"
	cs := s.(*store)
	dest := cs.cacheEntryDir(ref)
	// Pre-create "collide.raw" as a directory inside the cache entry so
	// os.OpenFile fails with EISDIR.
	if err := os.MkdirAll(filepath.Join(dest, "collide.raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := s.PullImage(context.Background(), ref, io.Discard)
	if err == nil {
		t.Fatal("expected error when output path is a directory")
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("error should mention 'create', got: %v", err)
	}
}

// TestPullImage_MkdirError exercises the os.MkdirAll failure branch in
// PullImage: the cache dir's parent is a regular file.
func TestPullImage_MkdirError(t *testing.T) {
	tmp := t.TempDir()
	asFile := filepath.Join(tmp, "cachedir-but-file")
	if err := os.WriteFile(asFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(asFile) // store dir is a regular file → MkdirAll fails
	err := s.PullImage(context.Background(), "https://example.com/x.raw", io.Discard)
	if err == nil {
		t.Fatal("expected mkdir error when store dir is a regular file")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("error should mention mkdir, got: %v", err)
	}
}

// TestValidateHTTPCache_ChecksumBodyReadError exercises the io.ReadAll
// error branch: the server advertises a large Content-Length then
// hijacks + closes the connection mid-body so the read fails.
func TestValidateHTTPCache_ChecksumBodyReadError(t *testing.T) {
	payload := []byte("hello")
	imageURL := "https://example.com/img.raw"
	s := New(t.TempDir())
	writeCacheFile(t, s, imageURL, payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000") // lie: claim 1MB
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short")) // send 5 bytes
		// Hijack + close so the client sees an unexpected EOF on ReadAll.
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	var w bytes.Buffer
	if s.(*store).validateHTTPCache(context.Background(), imageURL, srv.URL+"/CHECKSUM", &w) {
		t.Error("expected false when checksum body read fails")
	}
	// Either "read body" or "fetch" diagnostic is acceptable.
	out := w.String()
	if !strings.Contains(out, "read body") && !strings.Contains(out, "fetch") {
		t.Errorf("expected read/fetch diagnostic, got: %s", out)
	}
}

// TestValidateHTTPCache_BadChecksumURL covers the http.NewRequest error
// path with an unsupported scheme / malformed URL.
func TestValidateHTTPCache_BadChecksumURL(t *testing.T) {
	payload := []byte("hello")
	imageURL := "https://example.com/img.raw"
	s := New(t.TempDir())
	writeCacheFile(t, s, imageURL, payload)

	var w bytes.Buffer
	if s.(*store).validateHTTPCache(context.Background(), imageURL, "://invalid-url", &w) {
		t.Error("expected false for malformed checksum URL")
	}
	if !strings.Contains(w.String(), "build request") {
		t.Errorf("expected 'build request' diagnostic, got: %s", w.String())
	}
}

// TestValidateHTTPCache_ChecksumURLUnreachable covers the http.Do error
// path (server unreachable).
func TestValidateHTTPCache_ChecksumURLUnreachable(t *testing.T) {
	payload := []byte("hello")
	imageURL := "https://example.com/img.raw"
	s := New(t.TempDir())
	writeCacheFile(t, s, imageURL, payload)

	// Bind to a closed server.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	var w bytes.Buffer
	if s.(*store).validateHTTPCache(context.Background(), imageURL, url+"/CHECKSUM", &w) {
		t.Error("expected false when checksum server is down")
	}
	if !strings.Contains(w.String(), "fetch") {
		t.Errorf("expected 'fetch' diagnostic, got: %s", w.String())
	}
}

// TestPullImage_OCI_BadRepositoryRef surfaces the
// remote.NewRepository error branch via a malformed ref.
func TestPullImage_OCI_BadRepositoryRef(t *testing.T) {
	s := New(t.TempDir())
	// A ref with an invalid registry component makes NewRepository fail.
	ref := "oci://INVALID HOST WITH SPACES/repo:tag"
	err := s.PullImage(context.Background(), ref, io.Discard)
	if err == nil {
		t.Fatal("expected error for malformed OCI ref")
	}
}

// TestPullImage_OCI_OCIStoreError surfaces the oci.New(destDir) error
// branch by pre-creating index.json as a directory inside the cache
// entry so oci.New fails when it reads the index.
func TestPullImage_OCI_OCIStoreError(t *testing.T) {
	s := New(t.TempDir())
	ref := "registry.example.com/repo:tag"
	cs := s.(*store)
	dest := cs.cacheEntryDir(ref)
	if err := os.MkdirAll(filepath.Join(dest, "index.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.PullImage(ctx, ref, io.Discard)
	if err == nil {
		t.Fatal("expected error when oci.New cannot use destDir")
	}
}

