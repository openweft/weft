// PullKernel — fetches the shared microVM kernel binary from an OCI artifact
// (built and pushed by the openweft/weft-microvm-kernel CI workflow via
// ORAS) and writes it to $XDG_DATA_HOME/weft-microvm/kernel so the agent's
// RegisterMicroVM can copy it into each microVM's directory.
//
// The artifact uses two custom media types under the openweft vendor tree:
//
//	application/vnd.openweft.microvm.kernel        — manifest artifactType
//	application/vnd.openweft.microvm.kernel.image  — the raw Linux Image
//	application/vnd.openweft.microvm.kernel.config — the merged microVM
//	                                                  kernel config-fragment
//	                                                  carried for provenance
//
// We only need the .image layer; the config layer is ignored here (it lives in
// the registry for traceability — `oras manifest fetch …` is enough to inspect
// it). Matching by media type rather than artifactType keeps the puller
// tolerant of registries that don't yet surface artifactType to clients.

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

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// kernelLayerMediaType identifies the blob inside the kernel OCI artifact that
// holds the raw Linux Image (the binary the hypervisor loads). The CI workflow
// in openweft/weft-microvm-kernel pushes the layer with this media type.
const kernelLayerMediaType = "application/vnd.openweft.microvm.kernel.image"

// PullKernel resolves the OCI artifact reference, downloads the kernel layer,
// and atomically replaces $XDG_DATA_HOME/weft-microvm/kernel. Re-running with
// the same ref is a (cheap) re-download — content-addressable dedup is left
// to the underlying transport; here we just always overwrite.
//
// Mirrors the Pull + PullPodInitrd transport setup : honours
// WEFT_MICROVM_REGISTRY_MIRROR so a cluster-local zot serves the kernel
// artifact, and descends one level of OCI index when the published tag
// carries a multi-arch index (one manifest per arch).
func PullKernel(image string) error {
	ctx := context.Background()
	canonical := expandDockerHubShorthand(image)
	canonical = rewriteForMirror(canonical)
	repo, err := newRepository(canonical)
	if err != nil {
		return fmt.Errorf("parse %q: %w", canonical, err)
	}

	tag := repo.Reference.Reference
	if tag == "" {
		tag = "latest"
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", image, err)
	}
	// Descend a multi-platform index to the linux/<arch> entry, same as
	// Pull. Non-index descriptors pass through unchanged so callers that
	// publish a single-arch manifest see no behaviour change.
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

	// Pick the kernel layer by media type. The artifact may carry sibling
	// layers (e.g. the config-fragment for provenance) we don't need here.
	var kernelLayer *ocispec.Descriptor
	for i, l := range manifest.Layers {
		if l.MediaType == kernelLayerMediaType {
			kernelLayer = &manifest.Layers[i]
			break
		}
	}
	if kernelLayer == nil {
		return fmt.Errorf("no kernel layer (%s) in manifest of %s", kernelLayerMediaType, image)
	}

	dst := KernelPath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	tmp := dst + ".tmp"
	_ = os.Remove(tmp) // tidy any leftover .tmp from an interrupted earlier run
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	log.Printf("weft-microvm pull-kernel: %s → %s (%d bytes expected)", image, dst, kernelLayer.Size)

	rc, err := repo.Fetch(ctx, *kernelLayer)
	if err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("pull blob: %w", err)
	}
	n, copyErr := io.Copy(f, rc)
	_ = rc.Close()
	if cerr := f.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write blob: %w", copyErr)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, dst, err)
	}
	log.Printf("weft-microvm pull-kernel: done — %s (%d bytes)", dst, n)
	return nil
}
