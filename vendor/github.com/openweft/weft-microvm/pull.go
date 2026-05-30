// Pull — pull an OCI image, extract its rootfs to the local cache,
// and materialise a runtime-style process spec for weft-microvm-init at
// <rootfs>/.weft-microvm/config.json.
//
// What we DO:
//   - Resolve the reference (image-spec ref grammar).
//   - Fetch the per-platform manifest from the registry, picking
//     linux/<runtime arch>. The init we ship is Linux-only; we pin
//     amd64/arm64 to match the host so the kernel and the
//     userspace match.
//   - Pull each layer blob; recognise the standard gzipped+plain
//     OCI tar layer types; apply OCI whiteout rules during extract.
//   - Pull the config blob (small JSON), derive process spec,
//     write <rootfs>/.weft-microvm/config.json.
//
// What we do NOT do yet:
//   - Cosign signature verification (init/internal/cosign exists;
//     wiring it in is a roadmap item once the runner gains its own
//     trust-policy plumbing).
//   - Content addressable dedup (we cache by refsafe(image), not by
//     blob digest; two refs that share a layer re-download it).
//   - Authentication beyond the registry's anonymous tier.
//
// All concerns above are tracked as TODO comments next to the
// relevant call sites.

package microvm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/openweft/weft-microvm/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// expandDockerHubShorthand rewrites Docker Hub shorthand references
// (e.g. `alpine:3.21`, `library/alpine`, `myorg/myimage`) to fully-
// qualified registry URLs that init/pkg/oci's ParseRef accepts.
//
// Rules (mirroring the resolution `docker pull` performs):
//
//   - "name" or "name:tag"          → registry-1.docker.io/library/name[:tag]
//   - "owner/name" or "owner/name:tag" → registry-1.docker.io/owner/name[:tag]
//   - anything containing a "."  or ":" before the first "/" (a host)
//     is left untouched (already FQDN, e.g. ghcr.io/foo/bar, quay.io/baz,
//     registry.example.com:5000/svc/img).
//
// Returns the input verbatim when no rewrite applies.
func expandDockerHubShorthand(image string) string {
	slash := strings.Index(image, "/")
	if slash >= 0 {
		host := image[:slash]
		// A "real" host contains a "." (DNS) or a ":port" suffix.
		if strings.ContainsAny(host, ".:") {
			return image
		}
		// owner/name (Docker Hub user repo).
		return "registry-1.docker.io/" + image
	}
	// Bare name → library/<name> on Docker Hub.
	return "registry-1.docker.io/library/" + image
}

// Pull resolves `image`, downloads everything, and materialises the
// rootfs + .weft-microvm/config.json. Returns nil on success.
func Pull(image string) error {
	canonical := expandDockerHubShorthand(image)
	ref, err := oci.ParseRef(canonical)
	if err != nil {
		return fmt.Errorf("parse %q: %w", canonical, err)
	}

	rs := refsafe(image)
	root := imageRoot(rs)
	rootfs := rootfsPath(rs)

	// Idempotency sentinel: a successful prior Pull writes
	// .weft-microvm/config.json last, so its presence means the rootfs is
	// fully materialised for this ref — skip the fetch + extract entirely.
	// Re-extracting layers over an existing tree previously panicked mid-
	// stream (cp_file_range vs an extracted whiteout) and re-downloading
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

	c := oci.NewClient()
	// TODO: thread a cosign verifier through once the runner grows a
	// trust-policy flag (`--trusted-keys`, …). For now we trust the
	// registry blob digest chain end-to-end.
	manifest, manifestBytes, err := c.PullManifestForPlatform(ref, "linux", runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("pull manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), manifestBytes, 0o644); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	log.Printf("weft-microvm pull: %d layer(s) + 1 config", len(manifest.Layers))

	// Layers, in order — base first, topmost last. Each layer is
	// applied on top of the previous so whiteouts wipe what earlier
	// layers wrote.
	for i, layer := range manifest.Layers {
		log.Printf("weft-microvm pull:   layer %d/%d %s (%d B)", i+1, len(manifest.Layers), layer.Digest, layer.Size)
		if err := pullAndExtractLayer(c, ref, layer, rootfs); err != nil {
			return fmt.Errorf("layer %d (%s): %w", i, layer.Digest, err)
		}
	}

	// Config blob → process spec.
	var cfgBuf bytes.Buffer
	if _, err := c.PullBlob(ref, manifest.Config.Digest, &cfgBuf); err != nil {
		return fmt.Errorf("pull config blob: %w", err)
	}
	cfgBytes := cfgBuf.Bytes()
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

// pullAndExtractLayer streams one layer blob through the gzip+tar
// extractor in extract.go. We pull-and-extract in one pass rather
// than buffering the layer to disk first — saves both space (no
// extra .tar.gz copy on disk) and time (extract starts before the
// download finishes).
func pullAndExtractLayer(c *oci.Client, ref *oci.Ref, layer ocispec.Descriptor, rootfs string) error {
	// PullBlob writes to an io.Writer. We need an io.Reader to feed
	// extractLayer. Bridge with a pipe so the pull and the extract
	// run concurrently.
	pr, pw := io.Pipe()
	pullErr := make(chan error, 1)
	go func() {
		_, err := c.PullBlob(ref, layer.Digest, pw)
		_ = pw.CloseWithError(err)
		pullErr <- err
	}()
	if err := extractLayer(pr, rootfs); err != nil {
		// Drain the goroutine so it can exit cleanly.
		<-pullErr
		return fmt.Errorf("extract: %w", err)
	}
	if err := <-pullErr; err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	return nil
}
