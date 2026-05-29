//go:build darwin

package tartoci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-compressions/lzfse"
)

// Format is the Tart OCI disk image format (darwin only).
// A Format value satisfies the diskimage_format.Format interface defined in
// github.com/go-diskimages/interface without requiring an import of that
// module (Go structural typing).
//
// The "path" argument for Create and Detect is a directory in OCI image layout
// format; for ToRaw it is also such a directory (the cacheDir passed to
// ExtractDisk).
type Format struct{}

// Name returns "tart-oci".
func (Format) Name() string { return "tart-oci" }

// Create writes a new Tart OCI layout at path containing one LZFSE-compressed
// layer of sizeBytes of zeros.
//
// Note: the entire raw image is held in memory during compression. For sizes
// above a few hundred MiB this may consume significant memory; use Create only
// for testing or small bootstrap images.
func (Format) Create(path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("tart-oci: Create: size must be positive, got %d", sizeBytes)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("tart-oci: Create: mkdir: %w", err)
	}
	raw := make([]byte, sizeBytes)
	compressed, err := lzfse.Compress(raw)
	if err != nil {
		return fmt.Errorf("tart-oci: Create: compress: %w", err)
	}
	blobDigest, err := writeBlob(path, compressed)
	if err != nil {
		return err
	}
	manifestDigest, err := writeManifest(path, blobDigest, len(compressed), sizeBytes)
	if err != nil {
		return err
	}
	return writeIndex(path, manifestDigest)
}

// Detect returns (true, nil) if path is a directory with a valid Tart OCI
// layout (index.json present and at least one tart.disk.v2 layer reachable).
func (Format) Detect(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	idxData, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		return false, nil //nolint:nilerr // absence of index.json means not this format
	}
	var idx ociIndex
	if err := json.Unmarshal(idxData, &idx); err != nil || len(idx.Manifests) == 0 {
		return false, nil
	}
	return hasDiskLayer(path, idx.Manifests[0].Digest), nil
}

// ToRaw extracts the raw disk image from the Tart OCI layout at src and writes
// it to dst. Progress messages are written to w.
func (Format) ToRaw(src, dst string, w io.Writer) error {
	return ExtractDisk(src, dst, w)
}

// Resize is not supported for Tart OCI images: the disk layer is
// LZFSE-compressed and the manifest digest would need to be recomputed.
// Use Create to build a new image with the desired size.
func (Format) Resize(path string, newSizeBytes int64) error {
	return fmt.Errorf("tart-oci: Resize not supported; create a new image instead")
}

func writeBlob(layoutDir string, data []byte) (string, error) {
	h := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(h[:])
	blobDir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return "", fmt.Errorf("tart-oci: Create: mkdir blobs: %w", err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hex.EncodeToString(h[:])), data, 0o644); err != nil {
		return "", fmt.Errorf("tart-oci: Create: write blob: %w", err)
	}
	return digest, nil
}

func writeManifest(layoutDir, blobDigest string, blobSize int, sizeBytes int64) (string, error) {
	manifest := map[string]interface{}{
		"schemaVersion": 2,
		"layers": []map[string]interface{}{
			{
				"mediaType": DiskMediaType,
				"digest":    blobDigest,
				"size":      blobSize,
				"annotations": map[string]string{
					"org.cirruslabs.tart.uncompressed-size": fmt.Sprintf("%d", sizeBytes),
				},
			},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("tart-oci: Create: marshal manifest: %w", err)
	}
	digest, err := writeBlob(layoutDir, data)
	if err != nil {
		return "", err
	}
	return digest, nil
}

func writeIndex(layoutDir, manifestDigest string) error {
	index := map[string]interface{}{
		"schemaVersion": 2,
		"manifests": []map[string]interface{}{
			{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": manifestDigest},
		},
	}
	data, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("tart-oci: Create: marshal index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), data, 0o644); err != nil {
		return fmt.Errorf("tart-oci: Create: write index.json: %w", err)
	}
	return nil
}

func hasDiskLayer(layoutDir, manifestDigest string) bool {
	data, err := os.ReadFile(BlobPath(layoutDir, manifestDigest))
	if err != nil {
		return false
	}
	var manifest ociManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false
	}
	for _, l := range manifest.Layers {
		if l.MediaType == DiskMediaType {
			return true
		}
	}
	return false
}
