// Pull — pull an OCI image, extract its rootfs to the local cache, and
// materialise a runtime-style process spec for weft-microvm-init at
// <rootfs>/.weft-microvm/config.json.
//
// What we DO:
//   - Resolve the reference (image-spec ref grammar) via oras-go.
//   - Fetch the per-platform manifest, picking linux/<runtime arch>. The init
//     we ship is Linux-only; we pin the arch to match the host so the kernel
//     and userspace match.
//   - Pull each layer blob; recognise the standard gzipped+plain OCI tar
//     layer types; apply OCI whiteout rules during extract.
//   - Pull the config blob (small JSON), derive a process spec, write
//     <rootfs>/.weft-microvm/config.json.
//
// What we do NOT do yet:
//   - Cosign signature verification (roadmap item once the runner gains its
//     own trust-policy plumbing).
//   - Content-addressable dedup (we cache by refsafe(image), not by blob
//     digest; two refs that share a layer re-download it).
//   - Authentication beyond the registry's anonymous tier (oras-go's
//     auth.Client makes plumbing creds a one-liner once we need it).
//
// All concerns above are tracked as TODOs alongside the relevant call sites.

package microvm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
)

// expandDockerHubShorthand rewrites Docker Hub shorthand references
// (`alpine:3.21`, `library/alpine`, `myorg/myimage`) to fully-qualified
// registry URLs that oras-go's remote.NewRepository accepts.
//
// Rules (mirroring the resolution `docker pull` performs):
//
//   - "name" or "name:tag"             → registry-1.docker.io/library/name[:tag]
//   - "owner/name" or "owner/name:tag" → registry-1.docker.io/owner/name[:tag]
//   - "docker.io/…"                    → registry-1.docker.io/…  (docker.io is
//                                        the marketing site, not the registry;
//                                        single-component repos gain the
//                                        library/ prefix)
//   - anything else containing a "." or ":" before the first "/" (a host) is
//     left untouched (already FQDN, e.g. ghcr.io/foo/bar, quay.io/baz,
//     registry.example.com:5000/svc/img).
//
// oras-go accepts these forms as-is once normalised; doing the rewrite here
// keeps callers free to pass docker-style shorthands without thinking about
// it.
func expandDockerHubShorthand(image string) string {
	const dockerHubAlias = "docker.io/"
	if rest, ok := strings.CutPrefix(image, dockerHubAlias); ok {
		nameOnly := rest
		if i := strings.IndexAny(nameOnly, ":@"); i >= 0 {
			nameOnly = nameOnly[:i]
		}
		if !strings.Contains(nameOnly, "/") {
			return "registry-1.docker.io/library/" + rest
		}
		return "registry-1.docker.io/" + rest
	}
	slash := strings.Index(image, "/")
	if slash >= 0 {
		host := image[:slash]
		if strings.ContainsAny(host, ".:") {
			return image
		}
		return "registry-1.docker.io/" + image
	}
	return "registry-1.docker.io/library/" + image
}

// newRepository builds an oras-go remote.Repository, flipping PlainHTTP on for
// loopback hosts. Real registries (ghcr.io, registry-1.docker.io, zot in
// production) speak TLS; dev zots and the httptest servers our tests stand up
// on 127.0.0.1 do not, and `docker pull` extends the same courtesy. Keeping
// the rule narrow (loopback only) avoids accidentally downgrading a real
// hostname.
func newRepository(canonical string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(canonical)
	if err != nil {
		return nil, err
	}
	host := repo.Reference.Registry
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
		repo.PlainHTTP = true
	}
	return repo, nil
}

// Pull resolves `image`, downloads everything, and materialises the rootfs +
// .weft-microvm/config.json. Returns nil on success.
func Pull(image string) error {
	ctx := context.Background()
	canonical := expandDockerHubShorthand(image)
	repo, err := newRepository(canonical)
	if err != nil {
		return fmt.Errorf("parse %q: %w", canonical, err)
	}
	// Defaults are fine for the anonymous tier: oras-go negotiates the
	// registry's bearer token and follows redirects internally.

	rs := refsafe(image)
	root := imageRoot(rs)
	rootfs := rootfsPath(rs)

	// Idempotency sentinel: a successful prior Pull writes
	// .weft-microvm/config.json last, so its presence means the rootfs is
	// fully materialised for this ref — skip the fetch + extract entirely.
	// Re-extracting layers over an existing tree previously panicked mid-
	// stream (copy_file_range vs an extracted whiteout) and re-downloading
	// burns time + bandwidth; both are avoided here. Forced re-pull = the
	// caller `rm -rf` the cache entry first.
	sentinel := filepath.Join(rootfs, ".weft-microvm", "config.json")
	if _, err := os.Stat(sentinel); err == nil {
		log.Printf("weft-microvm pull: %s already cached at %s — skipping", image, root)
		return nil
	}
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", rootfs, err)
	}
	log.Printf("weft-microvm pull: %s -> %s", image, root)

	tag := repo.Reference.Reference
	if tag == "" {
		tag = "latest"
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", image, err)
	}
	// Descend a multi-platform index to the linux/<arch> entry (formerly
	// PullManifestForPlatform). Non-index descriptors pass through.
	desc, err = resolvePlatform(ctx, repo, desc, "linux", runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("resolve platform: %w", err)
	}

	manifestBytes, err := fetchAll(ctx, repo, desc)
	if err != nil {
		return fmt.Errorf("pull manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), manifestBytes, 0o644); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	log.Printf("weft-microvm pull: %d layer(s) + 1 config", len(manifest.Layers))

	// Layers, in order — base first, topmost last. Each layer is applied on
	// top of the previous so whiteouts wipe what earlier layers wrote.
	for i, layer := range manifest.Layers {
		log.Printf("weft-microvm pull:   layer %d/%d %s (%d B)", i+1, len(manifest.Layers), layer.Digest, layer.Size)
		if err := pullAndExtractLayer(ctx, repo, layer, rootfs); err != nil {
			return fmt.Errorf("layer %d (%s): %w", i, layer.Digest, err)
		}
	}

	// Config blob → process spec.
	cfgBytes, err := fetchAll(ctx, repo, manifest.Config)
	if err != nil {
		return fmt.Errorf("pull config blob: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfgBytes, 0o644); err != nil {
		return fmt.Errorf("save config.json: %w", err)
	}
	var img ocispec.Image
	if err := json.Unmarshal(cfgBytes, &img); err != nil {
		return fmt.Errorf("decode image config: %w", err)
	}
	proc, err := processFromImageConfig(img.Config)
	if err != nil {
		return err
	}
	out, err := marshalConfig(proc)
	if err != nil {
		return err
	}
	microvmDir := filepath.Join(rootfs, ".weft-microvm")
	if err := os.MkdirAll(microvmDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", microvmDir, err)
	}
	if err := os.WriteFile(filepath.Join(microvmDir, "config.json"), out, 0o644); err != nil {
		return fmt.Errorf("write .weft-microvm/config.json: %w", err)
	}

	log.Printf("weft-microvm pull: done — %s ready for Run", image)
	return nil
}

// pullAndExtractLayer streams one layer blob through the gzip+tar extractor
// in extract.go. We pull-and-extract in one pass — oras-go's repo.Fetch
// returns an io.ReadCloser we can feed straight in, no goroutine pipe needed
// (the previous implementation bridged a writer-only blob fetch into a
// reader; oras-go's API is already reader-shaped).
func pullAndExtractLayer(ctx context.Context, repo *remote.Repository, layer ocispec.Descriptor, rootfs string) error {
	rc, err := repo.Fetch(ctx, layer)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	defer rc.Close()
	if err := extractLayer(rc, rootfs); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	return nil
}

// resolvePlatform descends through a multi-platform index manifest, picking
// the entry that matches (os, arch). Non-index descriptors pass through
// unchanged so callers don't need to special-case the single-platform case.
func resolvePlatform(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor, os, arch string) (ocispec.Descriptor, error) {
	switch desc.MediaType {
	case ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.list.v2+json":
		body, err := fetchAll(ctx, repo, desc)
		if err != nil {
			return desc, err
		}
		var idx ocispec.Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return desc, fmt.Errorf("decode index: %w", err)
		}
		for _, m := range idx.Manifests {
			if m.Platform != nil && m.Platform.OS == os && m.Platform.Architecture == arch {
				return m, nil
			}
		}
		return desc, fmt.Errorf("no manifest matches %s/%s", os, arch)
	}
	return desc, nil
}

// fetchAll resolves an io.ReadCloser from repo.Fetch and reads it all into
// memory. Used for small blobs (manifests + config) where streaming buys
// nothing.
func fetchAll(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
