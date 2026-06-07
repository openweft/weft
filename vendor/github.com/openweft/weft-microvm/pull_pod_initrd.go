// PullPodInitrd — fetches the shared pod-mode initramfs from an OCI artifact
// (built and pushed by openweft/weft-microvm-init's release workflow via
// ORAS) and writes it to $XDG_DATA_HOME/weft-microvm/pod-initrd so the
// agent's pod boot path (locatePodBoot in pod.go) finds it without needing
// an operator to scp anything.
//
// Sibling of PullKernel ; same OCI client + transport, different media type +
// destination path. The artifact uses the openweft vendor tree :
//
//	application/vnd.openweft.microvm.pod-initrd        — manifest artifactType
//	application/vnd.openweft.microvm.pod-initrd.cpio.gz — the cpio.gz layer
//
// The published tag carries a multi-arch OCI index (one manifest per arch
// pointing at a single-platform cpio.gz layer) — we resolve the index, pick
// the platform descriptor matching runtime.GOARCH, fetch that manifest, then
// fetch the cpio.gz layer it points at.

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

// podInitrdLayerMediaType identifies the cpio.gz blob inside the artifact.
// The CI workflow in openweft/weft-microvm-init pushes the layer with this
// media type.
const podInitrdLayerMediaType = "application/vnd.openweft.microvm.pod-initrd.cpio.gz"

// PullPodInitrd resolves the OCI artifact reference, picks the per-arch
// manifest from its multi-arch index, downloads the cpio.gz layer, and
// atomically replaces $XDG_DATA_HOME/weft-microvm/pod-initrd.
//
// Mirrors the Pull + PullKernel transport setup : honours
// WEFT_MICROVM_REGISTRY_MIRROR so a cluster-local zot serves the
// pod-initrd artifact, and uses the shared resolvePlatform helper to
// descend a multi-arch OCI index (the published shape).
func PullPodInitrd(image string) error {
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
	// Pull and PullKernel. Non-index descriptors pass through unchanged.
	// The pod-initrd is Linux-only (it's a Linux initramfs), so pinning
	// linux/<host arch> matches what the artifact actually contains.
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

	// Pick the cpio.gz layer by media type.
	var initrdLayer *ocispec.Descriptor
	for i, l := range manifest.Layers {
		if l.MediaType == podInitrdLayerMediaType {
			initrdLayer = &manifest.Layers[i]
			break
		}
	}
	if initrdLayer == nil {
		return fmt.Errorf("no pod-initrd layer (%s) in manifest of %s", podInitrdLayerMediaType, image)
	}

	dst := PodInitrdPath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	log.Printf("weft-microvm pull-pod-initrd: %s → %s (%d bytes expected, arch=%s)",
		image, dst, initrdLayer.Size, runtime.GOARCH)

	rc, err := repo.Fetch(ctx, *initrdLayer)
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
	log.Printf("weft-microvm pull-pod-initrd: done — %s (%d bytes)", dst, n)
	return nil
}
