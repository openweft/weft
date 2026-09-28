package tartoci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-compressions/lz4"
)

// This file handles Tart images stored on disk in the OCI image-layout form (an
// index.json plus a blobs/<algo>/<hex> content-addressable store), which is what
// `tart pull` caches locally. The registry client above produces the same
// content over the network; this lets the Format adapter (format.go) operate on
// an already-cached image without any network access.

// BlobPath maps an OCI digest ("sha256:<hex>") to its path inside the layout's
// blobs directory. A digest without the expected "algo:" prefix is placed
// directly under blobs/.
func BlobPath(layoutDir, digest string) string {
	if len(digest) < 8 || digest[6] != ':' {
		return filepath.Join(layoutDir, "blobs", digest)
	}
	return filepath.Join(layoutDir, "blobs", digest[:6], digest[7:])
}

// readManifest loads index.json and the first manifest it references.
func readManifest(layoutDir string) (*Manifest, error) {
	idxData, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("tart-oci: read index.json: %w", err)
	}
	var idx index
	if err := json.Unmarshal(idxData, &idx); err != nil {
		return nil, fmt.Errorf("tart-oci: parse index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return nil, fmt.Errorf("tart-oci: index.json has no manifests")
	}
	mData, err := os.ReadFile(BlobPath(layoutDir, idx.Manifests[0].Digest))
	if err != nil {
		return nil, fmt.Errorf("tart-oci: read manifest %s: %w", idx.Manifests[0].Digest, err)
	}
	var m Manifest
	if err := json.Unmarshal(mData, &m); err != nil {
		return nil, fmt.Errorf("tart-oci: parse manifest: %w", err)
	}
	return &m, nil
}

// ExtractDisk reads the Tart OCI layout at layoutDir, decompresses every disk
// layer, and writes the assembled raw disk image to dstPath. Progress is
// reported to w.
func ExtractDisk(layoutDir, dstPath string, w io.Writer) error {
	m, err := readManifest(layoutDir)
	if err != nil {
		return err
	}
	disks := m.DiskLayers()
	if len(disks) == 0 {
		return fmt.Errorf("tart-oci: no %s layers in manifest", MediaTypeDisk)
	}
	out, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("tart-oci: create %s: %w", dstPath, err)
	}
	defer out.Close() //nolint:errcheck // Sync below reports the real write error

	_, _ = fmt.Fprintf(w, "extracting disk (%d layer(s))\n", len(disks))
	for i, layer := range disks {
		_, _ = fmt.Fprintf(w, "  layer %d/%d %s\n", i+1, len(disks), layer.Digest)
		if err := extractLayer(layoutDir, layer, out); err != nil {
			return err
		}
	}
	return fsyncFile(out)
}

func extractLayer(layoutDir string, layer Descriptor, out io.Writer) error {
	f, err := os.Open(BlobPath(layoutDir, layer.Digest))
	if err != nil {
		return fmt.Errorf("tart-oci: open blob %s: %w", layer.Digest, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	uncompHash := sha256.New()
	n, err := lz4.DecompressAppleStream(io.MultiWriter(out, uncompHash), f)
	if err != nil {
		return fmt.Errorf("tart-oci: decompress layer %s: %w", layer.Digest, err)
	}
	if want := layer.Annotations[AnnUncompressedDigest]; want != "" {
		if err := checkDigest("uncompressed", uncompHash, want); err != nil {
			return err
		}
	}
	if s := layer.Annotations[AnnUncompressedSize]; s != "" {
		want, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil {
			return fmt.Errorf("tart-oci: layer %s: bad %s %q: %w", layer.Digest, AnnUncompressedSize, s, perr)
		}
		if want != n {
			return fmt.Errorf("tart-oci: layer %s: decompressed %d bytes, annotation says %d", layer.Digest, n, want)
		}
	}
	return nil
}

// writeLayout writes a single-disk-layer Tart OCI layout at layoutDir whose disk
// decompresses to raw.
func writeLayout(layoutDir string, raw []byte) error {
	if err := os.MkdirAll(filepath.Join(layoutDir, "blobs", "sha256"), 0o755); err != nil {
		return fmt.Errorf("tart-oci: create layout: %w", err)
	}
	frame := encodeAppleFrame(raw)
	// These structs are fully controlled here and always marshal, so the errors
	// are ignored deliberately.
	mData, _ := json.Marshal(Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        Descriptor{MediaType: "application/vnd.oci.image.config.v1+json"},
		Layers: []Descriptor{{
			MediaType: MediaTypeDisk,
			Digest:    digest(frame),
			Size:      int64(len(frame)),
			Annotations: map[string]string{
				AnnUncompressedSize:   strconv.Itoa(len(raw)),
				AnnUncompressedDigest: digest(raw),
			},
		}},
	})
	idxData, _ := json.Marshal(index{Manifests: []Descriptor{{
		MediaType: MediaTypeOCIManifest, Digest: digest(mData), Size: int64(len(mData)),
	}}})

	for _, blob := range [][]byte{frame, mData} {
		if err := writeBlob(layoutDir, blob); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), idxData, 0o644); err != nil {
		return fmt.Errorf("tart-oci: write index.json: %w", err)
	}
	return nil
}

// writeBlob writes data under its own sha256 digest in the layout's blob store.
func writeBlob(layoutDir string, data []byte) error {
	if err := os.WriteFile(BlobPath(layoutDir, digest(data)), data, 0o644); err != nil {
		return fmt.Errorf("tart-oci: write blob: %w", err)
	}
	return nil
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// digest returns the "sha256:<hex>" content digest of b.
func digest(b []byte) string {
	return digestPrefix + hex.EncodeToString(sha256Sum(b))
}
