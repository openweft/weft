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
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/openweft/weft-microvm/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// kernelLayerMediaType identifies the blob inside the kernel OCI artifact that
// holds the raw Linux Image (the binary the hypervisor loads). The CI workflow
// in openweft/weft-microvm-kernel pushes the layer with this media type.
const kernelLayerMediaType = "application/vnd.openweft.microvm.kernel.image"

// PullKernel resolves the OCI artifact reference, downloads the kernel layer,
// and atomically replaces $XDG_DATA_HOME/weft-microvm/kernel. Re-running with
// the same ref is a (cheap) re-download — content-addressable dedup is left
// to the underlying oci client; here we just always overwrite.
func PullKernel(image string) error {
	canonical := expandDockerHubShorthand(image)
	ref, err := oci.ParseRef(canonical)
	if err != nil {
		return fmt.Errorf("parse %q: %w", canonical, err)
	}

	c := oci.NewClient()
	manifest, _, err := c.PullManifest(ref)
	if err != nil {
		return fmt.Errorf("pull manifest: %w", err)
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
	// Tidy any leftover .tmp from an interrupted earlier run.
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	log.Printf("weft-microvm pull-kernel: %s → %s (%d bytes expected)", image, dst, kernelLayer.Size)
	n, err := c.PullBlob(ref, kernelLayer.Digest, f)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pull blob: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, dst, err)
	}
	log.Printf("weft-microvm pull-kernel: done — %s (%d bytes)", dst, n)
	return nil
}
