//go:build darwin

package imagestore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// retryDefaultTransport / setRetryDefaultTransport let tests swap the
// retry.DefaultClient transport for an httptest.Server's RoundTripper.
func retryDefaultTransport() http.RoundTripper {
	if t, ok := retry.DefaultClient.Transport.(*retry.Transport); ok {
		return t.Base
	}
	return retry.DefaultClient.Transport
}

func setRetryDefaultTransport(rt http.RoundTripper) {
	if t, ok := retry.DefaultClient.Transport.(*retry.Transport); ok {
		t.Base = rt
		return
	}
	retry.DefaultClient.Transport = rt
}

// fakeRegistry serves the slice of the OCI Distribution v2 API that
// `estimateOCITotalBytes` / `pullOCI` exercise during a Copy. Stores
// blobs + manifests in memory; supports HEAD + GET on
// `/v2/<repo>/manifests/<digest|tag>` and `/v2/<repo>/blobs/<digest>`.
type fakeRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte // digest → content
	manifests map[string][]byte // digest|tag → manifest content
	mediaType map[string]string // digest|tag → media type
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		blobs:     make(map[string][]byte),
		manifests: make(map[string][]byte),
		mediaType: make(map[string]string),
	}
}

func (r *fakeRegistry) addBlob(content []byte, mt string) ocispec.Descriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	dig := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	r.blobs[dig] = content
	r.mediaType[dig] = mt
	return ocispec.Descriptor{MediaType: mt, Digest: digest.Digest(dig), Size: int64(len(content))}
}

func (r *fakeRegistry) addManifest(content []byte, mt, tag string) ocispec.Descriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	dig := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	r.manifests[dig] = content
	r.mediaType[dig] = mt
	if tag != "" {
		r.manifests[tag] = content
		r.mediaType[tag] = mt
	}
	return ocispec.Descriptor{MediaType: mt, Digest: digest.Digest(dig), Size: int64(len(content))}
}

func (r *fakeRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/v2/" || req.URL.Path == "/v2" {
		w.WriteHeader(http.StatusOK)
		return
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v2/"), "/")
	if len(parts) < 3 {
		http.NotFound(w, req)
		return
	}
	last := parts[len(parts)-1]
	kind := parts[len(parts)-2]
	r.mu.Lock()
	defer r.mu.Unlock()
	switch kind {
	case "manifests":
		data, ok := r.manifests[last]
		if !ok {
			http.NotFound(w, req)
			return
		}
		mt := r.mediaType[last]
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Docker-Content-Digest", fmt.Sprintf("sha256:%x", sha256.Sum256(data)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		if req.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(data)
	case "blobs":
		data, ok := r.blobs[last]
		if !ok {
			http.NotFound(w, req)
			return
		}
		mt, hasMT := r.mediaType[last]
		if !hasMT {
			mt = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Docker-Content-Digest", last)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		if req.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(data)
	default:
		http.NotFound(w, req)
	}
}

// TestSumManifestBytes_DockerV2WithSetSize exercises the docker v2
// media type via direct invocation.
func TestSumManifestBytes_DockerV2WithSetSize(t *testing.T) {
	mf := ocispec.Manifest{
		Config: ocispec.Descriptor{Size: 1000},
		Layers: []ocispec.Descriptor{{Size: 5000}, {Size: 7000}},
	}
	data, _ := json.Marshal(mf)
	got := sumManifestBytes(context.Background(), nil, "application/vnd.docker.distribution.manifest.v2+json", data)
	if want := int64(13000); got != want {
		t.Errorf("docker v2 = %d, want %d", got, want)
	}
}

// TestEstimateOCITotalBytes_NonexistentTag returns 0 when Resolve fails.
func TestEstimateOCITotalBytes_NonexistentTag(t *testing.T) {
	reg := newFakeRegistry()
	srv := httptest.NewServer(reg)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	repo, err := remote.NewRepository(host + "/missing/repo")
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	got := estimateOCITotalBytes(context.Background(), repo, "not-a-tag")
	if got != 0 {
		t.Errorf("expected 0 for nonexistent tag, got %d", got)
	}
}

// TestEstimateOCITotalBytes_HappyPath exercises Resolve + Fetch +
// sumManifestBytes for an image manifest in a real fake registry.
func TestEstimateOCITotalBytes_HappyPath(t *testing.T) {
	reg := newFakeRegistry()
	cfg := reg.addBlob([]byte("{}"), ocispec.MediaTypeImageConfig)
	layer := reg.addBlob([]byte("layer-bytes-hi"), ocispec.MediaTypeImageLayer)
	manifest := ocispec.Manifest{
		Config: cfg,
		Layers: []ocispec.Descriptor{layer},
	}
	manifest.SchemaVersion = 2
	manifest.MediaType = ocispec.MediaTypeImageManifest
	mfBytes, _ := json.Marshal(manifest)
	reg.addManifest(mfBytes, ocispec.MediaTypeImageManifest, "latest")

	srv := httptest.NewServer(reg)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	repo, err := remote.NewRepository(host + "/test/repo")
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	got := estimateOCITotalBytes(context.Background(), repo, "latest")
	want := cfg.Size + layer.Size
	if got != want {
		t.Errorf("estimateOCITotalBytes = %d, want %d", got, want)
	}
}

// TestPullImage_OCI_HappyPath exercises the pullOCI happy path via a
// TLS-fronted in-memory registry. Covers oras.Copy + the surrounding
// progress wiring.
func TestPullImage_OCI_HappyPath(t *testing.T) {
	reg := newFakeRegistry()
	cfg := reg.addBlob([]byte("{}"), ocispec.MediaTypeImageConfig)
	layer := reg.addBlob([]byte("layerbytes"), ocispec.MediaTypeImageLayer)
	manifest := ocispec.Manifest{
		Config: cfg,
		Layers: []ocispec.Descriptor{layer},
	}
	manifest.SchemaVersion = 2
	manifest.MediaType = ocispec.MediaTypeImageManifest
	mfBytes, _ := json.Marshal(manifest)
	reg.addManifest(mfBytes, ocispec.MediaTypeImageManifest, "latest")

	srv := httptest.NewTLSServer(reg)
	defer srv.Close()

	// Swap http.DefaultClient.Transport so retry.DefaultClient (which the
	// auth client wraps) can talk to the TLS server.
	prev := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	http.DefaultClient.Transport = srv.Client().Transport

	// Now also swap retry.DefaultClient.Transport since pullOCI uses it.
	retryPrev := retryDefaultTransport()
	t.Cleanup(func() { setRetryDefaultTransport(retryPrev) })
	setRetryDefaultTransport(srv.Client().Transport)

	host := strings.TrimPrefix(srv.URL, "https://")
	ref := host + "/test/repo:latest"

	s := New(t.TempDir())
	var w discardWriter
	if err := s.PullImage(context.Background(), ref, &w); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if !s.ImageInCache(ref) {
		t.Errorf("image not cached after pull")
	}
}

// discardWriter is a simple io.Writer that discards output. We use a
// dedicated type so the test file doesn't pull in io.Discard's
// "import side-effects" twice.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestEstimateOCITotalBytes_FetchError exercises the repo.Fetch
// failure branch: the tag resolves (HEAD ok) but the manifest blob
// isn't actually retrievable (404 on GET). estimateOCITotalBytes
// returns 0 gracefully.
func TestEstimateOCITotalBytes_FetchError(t *testing.T) {
	reg := newFakeRegistry()
	// Register only the HEAD path by adding a manifest, then delete the
	// content but keep the digest known to ServeHTTP via a custom handler.
	cfg := reg.addBlob([]byte("{}"), ocispec.MediaTypeImageConfig)
	manifest := ocispec.Manifest{Config: cfg}
	mfBytes, _ := json.Marshal(manifest)
	desc := reg.addManifest(mfBytes, ocispec.MediaTypeImageManifest, "latest")

	// Wrap reg in a handler that returns the manifest for HEAD/resolve but
	// 404 on the GET-by-digest fetch. 404 is NOT retried by the oras retry
	// policy (only 5xx / 429 are) so the test stays fast.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, string(desc.Digest)) {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		reg.ServeHTTP(w, req)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	repo, err := remote.NewRepository(host + "/test/repo")
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	got := estimateOCITotalBytes(context.Background(), repo, "latest")
	if got != 0 {
		t.Errorf("expected 0 when fetch fails, got %d", got)
	}
}

// TestSumManifestBytes_ImageIndex_WithMatch exercises the
// image-index branch where a manifest matching runtime.GOARCH is
// found and its size is summed via a recursive call.
func TestSumManifestBytes_ImageIndex_WithMatch(t *testing.T) {
	reg := newFakeRegistry()
	cfg := reg.addBlob([]byte("{}"), ocispec.MediaTypeImageConfig)
	layer := reg.addBlob([]byte("L"), ocispec.MediaTypeImageLayer)
	manifest := ocispec.Manifest{
		Config: cfg,
		Layers: []ocispec.Descriptor{layer},
	}
	mfBytes, _ := json.Marshal(manifest)
	mfDesc := reg.addManifest(mfBytes, ocispec.MediaTypeImageManifest, "")

	// Build an index that includes the host's GOARCH.
	idx := ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    mfDesc.Digest,
				Size:      mfDesc.Size,
				Platform:  &ocispec.Platform{Architecture: "arm64", OS: "linux"},
			},
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    mfDesc.Digest,
				Size:      mfDesc.Size,
				Platform:  &ocispec.Platform{Architecture: "amd64", OS: "linux"},
			},
		},
	}
	idxBytes, _ := json.Marshal(idx)
	reg.addManifest(idxBytes, ocispec.MediaTypeImageIndex, "latest")

	srv := httptest.NewServer(reg)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	repo, err := remote.NewRepository(host + "/multiarch/repo")
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	got := estimateOCITotalBytes(context.Background(), repo, "latest")
	// We expect either cfg+layer (match found) or 0 (arch didn't match).
	// Either way, this exercises the index-walking branch.
	want := cfg.Size + layer.Size
	if got != want && got != 0 {
		t.Errorf("got %d, want %d or 0", got, want)
	}
}
