//go:build darwin

// Package imagestore handles downloading, caching, and converting VM disk
// images for the Apple Virtualization.framework adapter.
//
// It supports two image source types:
//   - HTTP/HTTPS: plain file download, optionally validated via a checksum file.
//   - OCI: pulled with oras-go from any OCI-compatible registry (e.g. GHCR).
//
// The store wraps a cache directory on disk. Each image is stored under
// cacheDir/sanitizedRef/. HTTP images keep the downloaded file as-is; OCI
// images keep an OCI layout store (index.json + blobs/).
package imagestore

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// store manages the local image cache for a VZ adapter.
type store struct {
	dir       string
	mu        sync.RWMutex
	checksums map[string]string // imageURL → checksumURL
}

// New creates a store backed by dir (created on demand).
func New(dir string) ImageStore {
	return &store{dir: dir}
}

// SetChecksums stores validation URLs so PullImage can verify HTTP downloads.
func (s *store) SetChecksums(checksums map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checksums = checksums
}

// Dir returns the path to the cache directory.
func (s *store) Dir() string { return s.dir }

// SetDir updates the cache directory. Useful when SetPaths is called on the
// parent adapter after the store is already initialised.
func (s *store) SetDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dir = dir
}

// cacheEntryDir returns the cache directory for a specific image ref.
func (s *store) cacheEntryDir(ref string) string {
	return filepath.Join(s.dir, SanitizeRef(ref))
}

// PullImage downloads ref into the local cache, writing progress to w.
// HTTPS refs are fetched directly; OCI refs use oras-go.
func (s *store) PullImage(ctx context.Context, ref string, w io.Writer) error {
	dest := s.cacheEntryDir(ref)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("imagestore pull: mkdir %s: %w", dest, err)
	}
	if isHTTPRef(ref) {
		if s.ImageInCache(ref) {
			s.mu.RLock()
			csURL := s.checksums[ref]
			s.mu.RUnlock()
			if csURL == "" || s.validateHTTPCache(ctx, ref, csURL, w) {
				_, _ = fmt.Fprintf(w, "already cached %s\n", ref)
				_, _ = fmt.Fprintf(w, "100%%\n")
				return nil
			}
			_, _ = fmt.Fprintf(w, "checksum mismatch, re-downloading %s\n", ref)
		}
		return s.pullHTTP(ctx, ref, dest, w)
	}
	return s.pullOCI(ctx, ref, dest, w)
}

// ImageInCache reports whether ref is already present in the local cache.
func (s *store) ImageInCache(ref string) bool {
	dest := s.cacheEntryDir(ref)
	if isHTTPRef(ref) {
		entries, err := os.ReadDir(dest)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if !e.IsDir() {
				return true
			}
		}
		return false
	}
	// OCI layout store writes index.json when at least one image is present.
	_, err := os.Stat(filepath.Join(dest, "index.json"))
	return err == nil
}

// ListOCI returns cached OCI images as a list of property maps.
func (s *store) ListOCI() ([]map[string]interface{}, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var items []map[string]interface{}
	for _, e := range entries {
		if e.IsDir() {
			items = append(items, map[string]interface{}{
				"name":   UnsanitizeRef(e.Name()),
				"source": UnsanitizeRef(e.Name()),
			})
		}
	}
	return items, nil
}

// DeleteOCI removes a cached image directory.
func (s *store) DeleteOCI(name string) error {
	dest := s.cacheEntryDir(name)
	return os.RemoveAll(dest)
}

// ─── HTTP pull ────────────────────────────────────────────────────────────────

func isHTTPRef(ref string) bool {
	return strings.HasPrefix(ref, "https://")
}

// pullHTTP downloads ref into destDir, writing progress to w.
func (s *store) pullHTTP(ctx context.Context, ref, destDir string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return fmt.Errorf("imagestore http pull: build request %s: %w", ref, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("imagestore http pull: fetch %s: %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("imagestore http pull: fetch %s: HTTP %d", ref, resp.StatusCode)
	}
	fname := filepath.Base(strings.SplitN(ref, "?", 2)[0])
	outPath := filepath.Join(destDir, fname)
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("imagestore http pull: create %s: %w", outPath, err)
	}
	defer f.Close()
	var body io.Reader = resp.Body
	if resp.ContentLength > 0 {
		body = &progressReader{r: resp.Body, w: w, total: resp.ContentLength}
	}
	if _, err := io.Copy(f, body); err != nil {
		return fmt.Errorf("imagestore http pull: write %s: %w", outPath, err)
	}
	_, _ = fmt.Fprintf(w, "pulled %s\n", ref)
	return nil
}

// ─── HTTP checksum validation ─────────────────────────────────────────────────

// validateHTTPCache returns true when the cached file's hash matches the
// remote checksum file, false on any mismatch or error.
func (s *store) validateHTTPCache(ctx context.Context, imageURL, checksumURL string, w io.Writer) bool {
	fname := filepath.Base(strings.SplitN(imageURL, "?", 2)[0])
	localPath := filepath.Join(s.cacheEntryDir(imageURL), fname)

	f, err := os.Open(localPath)
	if err != nil {
		_, _ = fmt.Fprintf(w, "checksum: cannot open cached file %s: %v\n", localPath, err)
		return false
	}
	defer f.Close()

	h := checksumHashFor(checksumURL)
	if _, err := io.Copy(h, f); err != nil {
		_, _ = fmt.Fprintf(w, "checksum: hash computation failed for %s: %v\n", localPath, err)
		return false
	}
	localHash := hex.EncodeToString(h.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		_, _ = fmt.Fprintf(w, "checksum: build request %s: %v\n", checksumURL, err)
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(w, "checksum: fetch %s: %v\n", checksumURL, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(w, "checksum: fetch %s: HTTP %d\n", checksumURL, resp.StatusCode)
		return false
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		_, _ = fmt.Fprintf(w, "checksum: read body %s: %v\n", checksumURL, err)
		return false
	}
	remoteHash := ParseChecksumFile(string(data), fname)
	if remoteHash == "" {
		_, _ = fmt.Fprintf(w, "checksum: entry for %q not found in %s\n", fname, checksumURL)
		return false
	}
	if !strings.EqualFold(localHash, remoteHash) {
		_, _ = fmt.Fprintf(w, "checksum: mismatch for %s (local=%s… remote=%s…)\n", fname, localHash[:16], remoteHash[:16])
		return false
	}
	_, _ = fmt.Fprintf(w, "checksum ok: %s\n", fname)
	return true
}

// checksumHashFor returns SHA-512 for SHA512SUMS files, SHA-256 otherwise.
func checksumHashFor(checksumURL string) hash.Hash {
	base := strings.ToUpper(filepath.Base(strings.SplitN(checksumURL, "?", 2)[0]))
	if strings.Contains(base, "512") {
		return sha512.New()
	}
	return sha256.New()
}

// ParseChecksumFile extracts the hex hash for filename from the content of a
// checksum file. Supports both BSD ("SHA256 (file) = hash") and GNU
// coreutils ("hash  file") formats.
func ParseChecksumFile(content, filename string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// BSD: "SHA256 (filename) = hash"
		if strings.Contains(line, "("+filename+")") {
			if idx := strings.LastIndex(line, "="); idx >= 0 {
				return strings.TrimSpace(line[idx+1:])
			}
		}
		// GNU: "hash  filename" or "hash filename"
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[len(fields)-1] == filename {
			return fields[0]
		}
	}
	return ""
}

// ─── OCI pull (oras-go) ───────────────────────────────────────────────────────

func (s *store) pullOCI(ctx context.Context, ref, destDir string, w io.Writer) error {
	orasRef := strings.TrimPrefix(ref, "oci://")
	repo, err := remote.NewRepository(orasRef)
	if err != nil {
		return fmt.Errorf("imagestore oci pull: repository %s: %w", ref, err)
	}
	repo.Client = &auth.Client{
		Client: retry.DefaultClient,
		Cache:  auth.DefaultCache,
	}
	tag := "latest"
	if idx := strings.LastIndex(orasRef, ":"); idx != -1 {
		tag = orasRef[idx+1:]
	}
	underlying, err := oci.New(destDir)
	if err != nil {
		return fmt.Errorf("imagestore oci pull: oci store %s: %w", destDir, err)
	}
	totalBytes := estimateOCITotalBytes(ctx, repo, tag)
	ps := &progressOCIStore{Store: underlying, w: w, total: totalBytes}
	if _, err = oras.Copy(ctx, repo, tag, ps, tag, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("imagestore oci pull %s: %w", ref, err)
	}
	_, _ = fmt.Fprintf(w, "pulled %s\n", ref)
	return nil
}

// estimateOCITotalBytes returns the total compressed blob size for tag,
// or 0 on any error.
func estimateOCITotalBytes(ctx context.Context, repo *remote.Repository, tag string) int64 {
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return 0
	}
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return 0
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return 0
	}
	return sumManifestBytes(ctx, repo, desc.MediaType, data)
}

func sumManifestBytes(ctx context.Context, repo *remote.Repository, mediaType string, data []byte) int64 {
	switch mediaType {
	case ocispec.MediaTypeImageManifest,
		"application/vnd.docker.distribution.manifest.v2+json":
		var mf ocispec.Manifest
		if err := json.Unmarshal(data, &mf); err != nil {
			return 0
		}
		total := mf.Config.Size
		for _, l := range mf.Layers {
			total += l.Size
		}
		return total
	case ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.list.v2+json":
		var idx ocispec.Index
		if err := json.Unmarshal(data, &idx); err != nil {
			return 0
		}
		goArch := runtime.GOARCH
		for _, m := range idx.Manifests {
			if m.Platform == nil || m.Platform.Architecture != goArch {
				continue
			}
			rc, err := repo.Fetch(ctx, m)
			if err != nil {
				continue
			}
			d, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				continue
			}
			return sumManifestBytes(ctx, repo, m.MediaType, d)
		}
		return 0
	default:
		return 0
	}
}

// ─── Progress helpers ─────────────────────────────────────────────────────────

// progressReader wraps an io.Reader and emits "N%\n" lines to w.
type progressReader struct {
	r       io.Reader
	w       io.Writer
	total   int64
	read    int64
	lastPct int
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.read += int64(n)
		if pr.total > 0 {
			if pct := int(pr.read * 100 / pr.total); pct > pr.lastPct {
				pr.lastPct = pct
				_, _ = fmt.Fprintf(pr.w, "%d%%\n", pct)
			}
		}
	}
	return n, err
}

// progressOCIStore wraps *oci.Store and intercepts Push to emit "N%\n" lines.
type progressOCIStore struct {
	*oci.Store
	w       io.Writer
	total   int64
	mu      sync.Mutex
	written int64
	lastPct int
}

func (ps *progressOCIStore) Push(ctx context.Context, expected ocispec.Descriptor, r io.Reader) error {
	return ps.Store.Push(ctx, expected, &streamProgressReader{Reader: r, ps: ps})
}

type streamProgressReader struct {
	io.Reader
	ps *progressOCIStore
}

func (sr *streamProgressReader) Read(p []byte) (int, error) {
	n, err := sr.Reader.Read(p)
	if n > 0 {
		sr.ps.mu.Lock()
		sr.ps.written += int64(n)
		if sr.ps.total > 0 {
			if pct := int(sr.ps.written * 100 / sr.ps.total); pct > sr.ps.lastPct {
				sr.ps.lastPct = pct
				sr.ps.mu.Unlock()
				_, _ = fmt.Fprintf(sr.ps.w, "%d%%\n", pct)
				return n, err
			}
		}
		sr.ps.mu.Unlock()
	}
	return n, err
}
