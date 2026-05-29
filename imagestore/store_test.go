//go:build darwin

package imagestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"
)

// -- helpers -----------------------------------------------------------------

func progressNonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func sha512Hex(data []byte) string {
	h := sha512.Sum512(data)
	return hex.EncodeToString(h[:])
}

// writeCacheFile writes content under store.cacheEntryDir(imageURL)/basename.
func writeCacheFile(t *testing.T, s ImageStore, imageURL string, content []byte) string {
	t.Helper()
	cs := s.(*store) // safe: New always returns *store
	dir := cs.cacheEntryDir(imageURL)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fname := filepath.Base(strings.SplitN(imageURL, "?", 2)[0])
	path := filepath.Join(dir, fname)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// -- progressReader ----------------------------------------------------------

func TestProgressReader_EmitsPercentLines(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1000)
	var out bytes.Buffer
	pr := &progressReader{r: bytes.NewReader(data), w: &out, total: 1000}
	buf := make([]byte, 100)
	for {
		_, err := pr.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	lines := progressNonEmptyLines(out.String())
	if len(lines) == 0 {
		t.Fatal("no progress lines emitted")
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, "%") {
			t.Errorf("unexpected line %q: expected to end with %%", l)
		}
	}
	if last := lines[len(lines)-1]; last != "100%" {
		t.Errorf("last progress line = %q, want \"100%%\"", last)
	}
}

func TestProgressReader_Monotonic(t *testing.T) {
	data := bytes.Repeat([]byte("y"), 500)
	var out bytes.Buffer
	pr := &progressReader{r: bytes.NewReader(data), w: &out, total: 500}
	_, _ = io.Copy(io.Discard, pr)
	prev := 0
	for _, l := range progressNonEmptyLines(out.String()) {
		var n int
		fmt.Sscanf(l, "%d%%", &n)
		if n <= prev {
			t.Errorf("progress not monotonically increasing: %d after %d", n, prev)
		}
		prev = n
	}
}

func TestProgressReader_NoTotalNoLines(t *testing.T) {
	data := bytes.Repeat([]byte("z"), 200)
	var out bytes.Buffer
	pr := &progressReader{r: bytes.NewReader(data), w: &out, total: 0}
	_, _ = io.Copy(io.Discard, pr)
	if out.Len() != 0 {
		t.Errorf("expected no output when total=0, got %q", out.String())
	}
}

func TestProgressReader_NoDuplicatePercents(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 100)
	var out bytes.Buffer
	pr := &progressReader{r: bytes.NewReader(data), w: &out, total: 100}
	buf := make([]byte, 1)
	for {
		_, err := pr.Read(buf)
		if err == io.EOF {
			break
		}
	}
	seen := map[string]int{}
	for _, l := range progressNonEmptyLines(out.String()) {
		seen[l]++
		if seen[l] > 1 {
			t.Errorf("duplicate progress line %q", l)
		}
	}
}

// -- streamProgressReader / progressOCIStore ---------------------------------

func TestStreamProgressReader_EmitsOnRead(t *testing.T) {
	var out bytes.Buffer
	ps := &progressOCIStore{w: &out, total: 200}
	sr := &streamProgressReader{Reader: bytes.NewReader(bytes.Repeat([]byte("b"), 200)), ps: ps}
	buf := make([]byte, 20)
	for {
		_, err := sr.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	lines := progressNonEmptyLines(out.String())
	if len(lines) == 0 {
		t.Fatal("streamProgressReader emitted no progress lines")
	}
	if last := lines[len(lines)-1]; last != "100%" {
		t.Errorf("last progress = %q, want \"100%%\"", last)
	}
}

func TestStreamProgressReader_Monotonic(t *testing.T) {
	var out bytes.Buffer
	ps := &progressOCIStore{w: &out, total: 1000}
	sr := &streamProgressReader{Reader: bytes.NewReader(bytes.Repeat([]byte("c"), 1000)), ps: ps}
	_, _ = io.Copy(io.Discard, sr)
	prev := 0
	for _, l := range progressNonEmptyLines(out.String()) {
		var n int
		fmt.Sscanf(l, "%d%%", &n)
		if n <= prev {
			t.Errorf("not monotonic: %d after %d (line %q)", n, prev, l)
		}
		prev = n
	}
}

func TestStreamProgressReader_NoDuplicates(t *testing.T) {
	var out bytes.Buffer
	ps := &progressOCIStore{w: &out, total: 50}
	sr := &streamProgressReader{Reader: bytes.NewReader(bytes.Repeat([]byte("d"), 50)), ps: ps}
	buf := make([]byte, 1)
	for {
		_, err := sr.Read(buf)
		if err == io.EOF {
			break
		}
	}
	seen := map[string]bool{}
	for _, l := range progressNonEmptyLines(out.String()) {
		if seen[l] {
			t.Errorf("duplicate progress line %q", l)
		}
		seen[l] = true
	}
}

func TestStreamProgressReader_ConcurrentSafe(t *testing.T) {
	var out bytes.Buffer
	ps := &progressOCIStore{w: &out, total: 2000}
	chunk := bytes.Repeat([]byte("e"), 1000)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sr := &streamProgressReader{Reader: bytes.NewReader(chunk), ps: ps}
			_, _ = io.Copy(io.Discard, sr)
		}()
	}
	wg.Wait()
}

func TestProgressOCIStore_NoBytesNoProgress(t *testing.T) {
	var out bytes.Buffer
	ps := &progressOCIStore{w: &out, total: 0}
	sr := &streamProgressReader{Reader: bytes.NewReader(bytes.Repeat([]byte("f"), 100)), ps: ps}
	_, _ = io.Copy(io.Discard, sr)
	if out.Len() != 0 {
		t.Errorf("expected no output when total=0, got %q", out.String())
	}
}

func TestProgressOCIStore_PushDelegates(t *testing.T) {
	dir := t.TempDir()
	store, err := oci.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ps := &progressOCIStore{Store: store, w: &out, total: 0}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Size:      0,
	}
	_ = ps.Push(context.Background(), desc, bytes.NewReader(nil))
}

// -- sumManifestBytes --------------------------------------------------------

func TestSumManifestBytes_Manifest(t *testing.T) {
	mf := ocispec.Manifest{
		Config: ocispec.Descriptor{Size: 100},
		Layers: []ocispec.Descriptor{{Size: 400}, {Size: 500}},
	}
	data, _ := json.Marshal(mf)
	got := sumManifestBytes(context.Background(), nil, ocispec.MediaTypeImageManifest, data)
	if want := int64(1000); got != want {
		t.Errorf("sumManifestBytes = %d, want %d", got, want)
	}
}

func TestSumManifestBytes_DockerV2Manifest(t *testing.T) {
	mf := ocispec.Manifest{
		Config: ocispec.Descriptor{Size: 50},
		Layers: []ocispec.Descriptor{{Size: 200}},
	}
	data, _ := json.Marshal(mf)
	got := sumManifestBytes(context.Background(), nil, "application/vnd.docker.distribution.manifest.v2+json", data)
	if want := int64(250); got != want {
		t.Errorf("sumManifestBytes docker = %d, want %d", got, want)
	}
}

func TestSumManifestBytes_UnknownMediaType(t *testing.T) {
	got := sumManifestBytes(context.Background(), nil, "application/unknown", []byte("{}"))
	if got != 0 {
		t.Errorf("expected 0 for unknown media type, got %d", got)
	}
}

func TestSumManifestBytes_MalformedJSON(t *testing.T) {
	got := sumManifestBytes(context.Background(), nil, ocispec.MediaTypeImageManifest, []byte("not-json"))
	if got != 0 {
		t.Errorf("expected 0 for malformed JSON, got %d", got)
	}
}

func TestSumManifestBytes_ZeroLayerSizes(t *testing.T) {
	mf := ocispec.Manifest{
		Config: ocispec.Descriptor{Size: 0},
		Layers: []ocispec.Descriptor{{Size: 0}, {Size: 0}},
	}
	data, _ := json.Marshal(mf)
	got := sumManifestBytes(context.Background(), nil, ocispec.MediaTypeImageManifest, data)
	if got != 0 {
		t.Errorf("expected 0 for zero-sized blobs, got %d", got)
	}
}

// -- checksumHashFor ---------------------------------------------------------

func TestChecksumHashFor_SHA512(t *testing.T) {
	cases := []string{
		"https://example.com/SHA512SUMS",
		"https://example.com/sha512sum",
		"https://example.com/images/SHA512SUMS.txt",
	}
	for _, url := range cases {
		h := checksumHashFor(url)
		h.Write([]byte("test"))
		if got := len(h.Sum(nil)); got != 64 {
			t.Errorf("checksumHashFor(%q): output size = %d, want 64 (SHA-512)", url, got)
		}
	}
}

func TestChecksumHashFor_SHA256(t *testing.T) {
	cases := []string{
		"https://example.com/CHECKSUM",
		"https://example.com/SHA256SUMS",
		"https://example.com/sha256sum",
		"https://example.com/checksum.txt",
	}
	for _, url := range cases {
		h := checksumHashFor(url)
		h.Write([]byte("test"))
		if got := len(h.Sum(nil)); got != 32 {
			t.Errorf("checksumHashFor(%q): output size = %d, want 32 (SHA-256)", url, got)
		}
	}
}

// -- ParseChecksumFile -------------------------------------------------------

func TestParseChecksumFile_BSDFormat(t *testing.T) {
	content := "SHA256 (myimage.raw) = abc123def456\n"
	got := ParseChecksumFile(content, "myimage.raw")
	if got != "abc123def456" {
		t.Errorf("ParseChecksumFile BSD = %q, want %q", got, "abc123def456")
	}
}

func TestParseChecksumFile_GNUFormat(t *testing.T) {
	content := "abc123def456  myimage.raw\n"
	got := ParseChecksumFile(content, "myimage.raw")
	if got != "abc123def456" {
		t.Errorf("ParseChecksumFile GNU = %q, want %q", got, "abc123def456")
	}
}

func TestParseChecksumFile_GNUFormatSingleSpace(t *testing.T) {
	content := "abc123def456 myimage.raw\n"
	got := ParseChecksumFile(content, "myimage.raw")
	if got != "abc123def456" {
		t.Errorf("ParseChecksumFile GNU single space = %q, want %q", got, "abc123def456")
	}
}

func TestParseChecksumFile_MultipleEntries(t *testing.T) {
	content := "SHA256 (other-image.raw) = 111111\nSHA256 (myimage.raw) = abc123\nSHA256 (another.qcow2) = 999999\n"
	got := ParseChecksumFile(content, "myimage.raw")
	if got != "abc123" {
		t.Errorf("ParseChecksumFile multi = %q, want %q", got, "abc123")
	}
}

func TestParseChecksumFile_CommentsIgnored(t *testing.T) {
	content := "# This is a comment\n# myimage.raw has checksum ffffffff\nabc123  myimage.raw\n"
	got := ParseChecksumFile(content, "myimage.raw")
	if got != "abc123" {
		t.Errorf("ParseChecksumFile comments = %q, want %q", got, "abc123")
	}
}

func TestParseChecksumFile_NotFound(t *testing.T) {
	content := "SHA256 (other.raw) = abc123\nhash  something.else\n"
	got := ParseChecksumFile(content, "myimage.raw")
	if got != "" {
		t.Errorf("ParseChecksumFile not found = %q, want %q", got, "")
	}
}

func TestParseChecksumFile_Empty(t *testing.T) {
	got := ParseChecksumFile("", "myimage.raw")
	if got != "" {
		t.Errorf("ParseChecksumFile empty = %q, want %q", got, "")
	}
}

func TestParseChecksumFile_RockyStyle(t *testing.T) {
	content := "# Rocky-10-GenericCloud-Base.latest.aarch64.qcow2: 507248640 bytes\nSHA256 (Rocky-10-GenericCloud-Base.latest.aarch64.qcow2) = abc0023cf8fb415532138fa735a79bc254ba0d13f4d79b032a09431d438ee36d\n"
	got := ParseChecksumFile(content, "Rocky-10-GenericCloud-Base.latest.aarch64.qcow2")
	if got != "abc0023cf8fb415532138fa735a79bc254ba0d13f4d79b032a09431d438ee36d" {
		t.Errorf("ParseChecksumFile Rocky = %q", got)
	}
}

func TestParseChecksumFile_DebianStyle(t *testing.T) {
	content := "b43ffa01ffa5767be8b164d210505b87c651a17228dc76870c05eb74d7125018db30dfb96c1cdac72f5ae4d781e226e32264a47f63835984a205b462e886fd65  debian-13-genericcloud-amd64.raw\n"
	got := ParseChecksumFile(content, "debian-13-genericcloud-amd64.raw")
	if got != "b43ffa01ffa5767be8b164d210505b87c651a17228dc76870c05eb74d7125018db30dfb96c1cdac72f5ae4d781e226e32264a47f63835984a205b462e886fd65" {
		t.Errorf("ParseChecksumFile Debian = %q", got)
	}
}

// -- validateHTTPCache -------------------------------------------------------

func TestValidateHTTPCache_SHA256Match(t *testing.T) {
	payload := []byte("fake image content sha256")
	imageURL := "https://example.com/images/myimage.qcow2"

	s := New(t.TempDir())
	writeCacheFile(t, s, imageURL, payload)

	checkContent := fmt.Sprintf("SHA256 (myimage.qcow2) = %s\n", sha256Hex(payload))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checkContent)
	}))
	defer srv.Close()

	var w bytes.Buffer
	if !s.(*store).validateHTTPCache(context.Background(), imageURL, srv.URL+"/CHECKSUM", &w) {
		t.Errorf("expected validateHTTPCache to return true (SHA-256 match); diag: %s", w.String())
	}
}

func TestValidateHTTPCache_SHA512Match(t *testing.T) {
	payload := []byte("fake image content sha512")
	imageURL := "https://example.com/images/myimage.raw"

	s := New(t.TempDir())
	writeCacheFile(t, s, imageURL, payload)

	checkContent := fmt.Sprintf("%s  myimage.raw\n", sha512Hex(payload))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checkContent)
	}))
	defer srv.Close()

	var w bytes.Buffer
	if !s.(*store).validateHTTPCache(context.Background(), imageURL, srv.URL+"/SHA512SUMS", &w) {
		t.Errorf("expected validateHTTPCache to return true (SHA-512 match); diag: %s", w.String())
	}
}

func TestValidateHTTPCache_HashMismatch(t *testing.T) {
	payload := []byte("real content")
	imageURL := "https://example.com/images/myimage.qcow2"

	s := New(t.TempDir())
	writeCacheFile(t, s, imageURL, payload)

	checkContent := "SHA256 (myimage.qcow2) = 0000000000000000000000000000000000000000000000000000000000000000\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checkContent)
	}))
	defer srv.Close()

	var w bytes.Buffer
	if s.(*store).validateHTTPCache(context.Background(), imageURL, srv.URL+"/CHECKSUM", &w) {
		t.Error("expected validateHTTPCache to return false on hash mismatch")
	}
	if !strings.Contains(w.String(), "mismatch") {
		t.Errorf("expected mismatch message in output, got: %s", w.String())
	}
}

func TestValidateHTTPCache_FileNotCached(t *testing.T) {
	imageURL := "https://example.com/images/missing.qcow2"
	s := New(t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "SHA256 (missing.qcow2) = abc\n")
	}))
	defer srv.Close()

	var w bytes.Buffer
	if s.(*store).validateHTTPCache(context.Background(), imageURL, srv.URL+"/CHECKSUM", &w) {
		t.Error("expected validateHTTPCache to return false when file is not cached")
	}
}

func TestValidateHTTPCache_EntryNotFoundInChecksumFile(t *testing.T) {
	payload := []byte("content")
	imageURL := "https://example.com/images/myimage.qcow2"

	s := New(t.TempDir())
	writeCacheFile(t, s, imageURL, payload)

	checkContent := "SHA256 (other-image.qcow2) = abc123\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checkContent)
	}))
	defer srv.Close()

	var w bytes.Buffer
	if s.(*store).validateHTTPCache(context.Background(), imageURL, srv.URL+"/CHECKSUM", &w) {
		t.Error("expected validateHTTPCache to return false when entry not found")
	}
	if !strings.Contains(w.String(), "not found") {
		t.Errorf("expected 'not found' message in output, got: %s", w.String())
	}
}

func TestValidateHTTPCache_ChecksumServerError(t *testing.T) {
	payload := []byte("content")
	imageURL := "https://example.com/images/myimage.qcow2"

	s := New(t.TempDir())
	writeCacheFile(t, s, imageURL, payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	var w bytes.Buffer
	if s.(*store).validateHTTPCache(context.Background(), imageURL, srv.URL+"/CHECKSUM", &w) {
		t.Error("expected validateHTTPCache to return false on HTTP 404")
	}
	if !strings.Contains(w.String(), "404") {
		t.Errorf("expected HTTP 404 message in output, got: %s", w.String())
	}
}

func TestStore_Dir(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if got := s.Dir(); got != dir {
		t.Errorf("Dir() = %q, want %q", got, dir)
	}
}

func TestStore_SetDir(t *testing.T) {
	s := New(t.TempDir())
	newDir := t.TempDir()
	s.SetDir(newDir)
	if got := s.Dir(); got != newDir {
		t.Errorf("after SetDir, Dir() = %q, want %q", got, newDir)
	}
}

func TestStore_ImageInCache_Absent(t *testing.T) {
	s := New(t.TempDir())
	if s.ImageInCache("https://example.com/missing.qcow2") {
		t.Error("expected ImageInCache to return false for absent image")
	}
}

func TestStore_ImageInCache_Present(t *testing.T) {
	s := New(t.TempDir())
	ref := "https://example.com/images/myimage.qcow2"
	writeCacheFile(t, s, ref, []byte("fake content"))
	if !s.ImageInCache(ref) {
		t.Error("expected ImageInCache to return true when file is cached")
	}
}

func TestStore_SetChecksums(t *testing.T) {
	s := New(t.TempDir())
	checksums := map[string]string{
		"https://example.com/a.qcow2": "https://example.com/SHA256SUMS",
	}
	s.SetChecksums(checksums)
	// Verify by running a validateHTTPCache lookup — the checksum URL must be
	// reachable to match, so we just confirm the method doesn't panic or error
	// when the cache file is absent (returns false).
	var w bytes.Buffer
	ok := s.(*store).validateHTTPCache(
		context.Background(),
		"https://example.com/a.qcow2",
		"https://example.com/SHA256SUMS",
		&w,
	)
	if ok {
		t.Error("expected false when cache file is absent")
	}
}
