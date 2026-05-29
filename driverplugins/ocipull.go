package driverplugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// dockerManifestList is the legacy media type for multi-arch indexes; GHCR may
// serve either it or the OCI image index.
const dockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

// pullExecutable fetches the per-platform binary layer of the OCI artifact at
// ref into cacheDir/execName, chmod +x, and returns the path. It caches by the
// layer's content digest (cache hit ⇒ no blob download) and tolerates an
// unreachable registry by reusing a prior cached copy.
func pullExecutable(ctx context.Context, ref, cacheDir, execName, token string) (string, error) {
	if cacheDir == "" {
		return "", fmt.Errorf("no plugin cache dir configured")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("plugin cache dir: %w", err)
	}
	dest := filepath.Join(cacheDir, execName)
	digestFile := dest + ".digest"

	repo, tag, err := newRepo(ref, token)
	if err != nil {
		return "", err
	}
	layer, rerr := resolveBinaryLayer(ctx, repo, tag)
	if rerr != nil {
		if isExecutable(dest) { // offline: a previously pulled copy is good enough
			return dest, nil
		}
		return "", rerr
	}
	if isExecutable(dest) && readTrim(digestFile) == layer.Digest.String() {
		return dest, nil // cache hit
	}

	rc, err := repo.Fetch(ctx, layer)
	if err != nil {
		return "", fmt.Errorf("fetch binary layer %s: %w", layer.Digest, err)
	}
	defer rc.Close()

	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("write plugin binary: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	_ = os.WriteFile(digestFile, []byte(layer.Digest.String()), 0o644)
	return dest, nil
}

// newRepo builds an oras remote repository for ref, anonymous unless a token is
// given. Returns the tag/digest reference to resolve.
func newRepo(ref, token string) (*remote.Repository, string, error) {
	ref = strings.TrimPrefix(ref, "oci://")
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, "", fmt.Errorf("bad OCI ref %q: %w", ref, err)
	}
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if token != "" {
		client.Credential = auth.StaticCredential(repo.Reference.Registry,
			auth.Credential{Username: "weft", Password: token})
	}
	repo.Client = client
	return repo, repo.Reference.Reference, nil
}

// resolveBinaryLayer resolves ref to the single binary layer for the running
// GOOS/GOARCH: it follows a multi-arch index to the matching image manifest,
// then returns that manifest's first (only) layer — the executable.
func resolveBinaryLayer(ctx context.Context, repo *remote.Repository, tag string) (ocispec.Descriptor, error) {
	root, b, err := oras.FetchBytes(ctx, repo, tag, oras.DefaultFetchBytesOptions)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if root.MediaType == ocispec.MediaTypeImageIndex || root.MediaType == dockerManifestList {
		var idx ocispec.Index
		if err := json.Unmarshal(b, &idx); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("parse image index: %w", err)
		}
		md, ok := pickPlatform(idx.Manifests)
		if !ok {
			return ocispec.Descriptor{}, fmt.Errorf("no manifest for %s/%s in index", runtime.GOOS, runtime.GOARCH)
		}
		if b, err = fetchAll(ctx, repo, md); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("fetch platform manifest: %w", err)
		}
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("parse image manifest: %w", err)
	}
	if len(m.Layers) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("manifest has no layers (expected the plugin binary)")
	}
	return m.Layers[0], nil
}

func pickPlatform(ms []ocispec.Descriptor) (ocispec.Descriptor, bool) {
	for _, d := range ms {
		if d.Platform != nil && d.Platform.OS == runtime.GOOS && d.Platform.Architecture == runtime.GOARCH {
			return d, true
		}
	}
	return ocispec.Descriptor{}, false
}

func fetchAll(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
