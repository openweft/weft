//go:build darwin

// Package tartoci extracts raw disk images from Tart-compatible OCI layout
// stores (e.g. ghcr.io/cirruslabs/*).
//
// Layer format: application/vnd.cirruslabs.tart.disk.v2
// Each blob is an LZFSE-compressed chunk of exactly
// `org.cirruslabs.tart.uncompressed-size` bytes (typically 512 MiB).
package tartoci

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-compressions/lzfse"
)

// DiskMediaType is the OCI media type for Tart disk layers.
const DiskMediaType = "application/vnd.cirruslabs.tart.disk.v2"

type ociIndex struct {
	Manifests []struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
	} `json:"manifests"`
}

type ociManifest struct {
	Layers []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
}

// ExtractDisk reads the OCI layout at cacheDir, finds all tart.disk.v2
// layers, decompresses each with LZFSE, and writes the concatenated raw
// disk image to dst. Progress messages are written to w.
func ExtractDisk(cacheDir, dst string, w io.Writer) error {
	idxData, err := os.ReadFile(filepath.Join(cacheDir, "index.json"))
	if err != nil {
		return fmt.Errorf("oci: read index.json: %w", err)
	}
	var idx ociIndex
	if err := json.Unmarshal(idxData, &idx); err != nil {
		return fmt.Errorf("oci: parse index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return fmt.Errorf("oci: index.json has no manifests")
	}
	manifestDigest := idx.Manifests[0].Digest
	manifestBlob := BlobPath(cacheDir, manifestDigest)

	manifestData, err := os.ReadFile(manifestBlob)
	if err != nil {
		return fmt.Errorf("oci: read manifest %s: %w", manifestDigest, err)
	}
	var manifest ociManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("oci: parse manifest: %w", err)
	}

	var diskLayers []struct {
		digest          string
		uncompressedLen int64
	}
	for _, l := range manifest.Layers {
		if l.MediaType != DiskMediaType {
			continue
		}
		uncompLen, err := strconv.ParseInt(
			l.Annotations["org.cirruslabs.tart.uncompressed-size"], 10, 64)
		if err != nil {
			return fmt.Errorf("oci: layer %s missing uncompressed-size: %w", l.Digest, err)
		}
		diskLayers = append(diskLayers, struct {
			digest          string
			uncompressedLen int64
		}{l.Digest, uncompLen})
	}
	if len(diskLayers) == 0 {
		return fmt.Errorf("oci: no %s layers found in manifest", DiskMediaType)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("oci: open output: %w", err)
	}
	defer out.Close()

	total := len(diskLayers)
	_, _ = fmt.Fprintf(w, "extracting OCI disk (%d chunk(s))…\n", total)

	for i, layer := range diskLayers {
		_, _ = fmt.Fprintf(w, "  chunk %d/%d (%.0f MiB uncompressed)…\n",
			i+1, total, float64(layer.uncompressedLen)/(1<<20))

		compressed, err := os.ReadFile(BlobPath(cacheDir, layer.digest))
		if err != nil {
			return fmt.Errorf("oci: read blob %s: %w", layer.digest, err)
		}

		decoded, err := decompressLZFSE(compressed)
		if err != nil {
			return fmt.Errorf("oci: decompress layer %d: %w", i+1, err)
		}

		if _, err := out.Write(decoded); err != nil {
			return fmt.Errorf("oci: write layer %d: %w", i+1, err)
		}
	}
	return nil
}

// decompressLZFSE decompresses src (LZFSE) and returns the raw bytes.
func decompressLZFSE(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, fmt.Errorf("empty source")
	}
	dst, err := lzfse.Decompress(src)
	if err != nil {
		return nil, fmt.Errorf("LZFSE decode: %w", err)
	}
	return dst, nil
}

// BlobPath converts an OCI digest string ("sha256:<hex>") to its path
// in the OCI layout's blobs directory.
func BlobPath(cacheDir, digest string) string {
	if len(digest) < 7 || digest[6] != ':' {
		return filepath.Join(cacheDir, "blobs", digest)
	}
	algo := digest[:6]
	hexStr := digest[7:]
	return filepath.Join(cacheDir, "blobs", algo, hexStr)
}
