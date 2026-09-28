package tartoci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-compressions/lz4"
)

// fsyncFile flushes f to stable storage. It is a variable so tests can inject a
// flush failure (which is otherwise not reliably reproducible on a real file).
var fsyncFile = func(f *os.File) error { return f.Sync() }

// PullDisk pulls the image at ref (e.g. "ghcr.io/cirruslabs/macos-sequoia-base:latest")
// and writes its assembled raw disk image to dstPath. Progress is reported to w
// (use io.Discard for none).
//
// Each disk layer is downloaded, verified against its sha256 blob digest,
// decompressed from its Apple LZ4 frame, and verified against its declared
// uncompressed size and content digest before being appended to the output, so
// the result is a byte-exact raw disk.
func PullDisk(ctx context.Context, ref, dstPath string, w io.Writer, opts ...Option) error {
	parsed, err := ParseReference(ref)
	if err != nil {
		return err
	}
	reg := NewRegistry(parsed, opts...)
	m, err := reg.Manifest(ctx)
	if err != nil {
		return err
	}
	out, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("tart-oci: create %s: %w", dstPath, err)
	}
	defer out.Close() //nolint:errcheck // Sync below reports the real write error
	if err := reg.writeDisk(ctx, m, out, w); err != nil {
		return err
	}
	if err := fsyncFile(out); err != nil {
		return fmt.Errorf("tart-oci: sync %s: %w", dstPath, err)
	}
	return nil
}

// Pull materializes a full Tart VM bundle at destDir: disk.img (the raw disk),
// config.json (the VM config), and, when present, nvram.bin (the NVRAM store).
// Every blob is verified against its digest. Progress is reported to w.
func Pull(ctx context.Context, ref, destDir string, w io.Writer, opts ...Option) error {
	parsed, err := ParseReference(ref)
	if err != nil {
		return err
	}
	reg := NewRegistry(parsed, opts...)
	m, err := reg.Manifest(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("tart-oci: create %s: %w", destDir, err)
	}

	if cfg := m.layersOf(MediaTypeConfig); len(cfg) > 0 {
		if err := reg.fetchRawBlob(ctx, cfg[0], filepath.Join(destDir, "config.json")); err != nil {
			return err
		}
	}
	if nv := m.layersOf(MediaTypeNVRAM); len(nv) > 0 {
		if err := reg.fetchRawBlob(ctx, nv[0], filepath.Join(destDir, "nvram.bin")); err != nil {
			return err
		}
	}

	diskPath := filepath.Join(destDir, "disk.img")
	out, err := os.Create(diskPath)
	if err != nil {
		return fmt.Errorf("tart-oci: create %s: %w", diskPath, err)
	}
	defer out.Close() //nolint:errcheck // Sync below reports the real write error
	if err := reg.writeDisk(ctx, m, out, w); err != nil {
		return err
	}
	if err := fsyncFile(out); err != nil {
		return fmt.Errorf("tart-oci: sync %s: %w", diskPath, err)
	}
	return nil
}

// writeDisk downloads, verifies and decompresses every disk layer of m to out.
func (r *Registry) writeDisk(ctx context.Context, m *Manifest, out io.Writer, w io.Writer) error {
	disks := m.DiskLayers()
	if len(disks) == 0 {
		return fmt.Errorf("tart-oci: manifest has no %s layers", MediaTypeDisk)
	}
	_, _ = fmt.Fprintf(w, "pulling disk (%d layer(s))\n", len(disks))
	for i, layer := range disks {
		_, _ = fmt.Fprintf(w, "  layer %d/%d %s\n", i+1, len(disks), layer.Digest)
		if err := r.writeDiskLayer(ctx, layer, out); err != nil {
			return err
		}
	}
	return nil
}

// writeDiskLayer streams one disk layer to out, verifying the compressed blob
// digest, the decompressed content digest, and the decompressed size.
func (r *Registry) writeDiskLayer(ctx context.Context, layer Descriptor, out io.Writer) error {
	body, err := r.blob(ctx, layer.Digest)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // read-only body

	compHash := sha256.New()
	uncompHash := sha256.New()
	src := io.TeeReader(body, compHash)    // hash the compressed bytes we read
	dst := io.MultiWriter(out, uncompHash) // hash the decompressed bytes we write
	n, err := lz4.DecompressAppleStream(dst, src)
	if err != nil {
		return fmt.Errorf("tart-oci: decompress layer %s: %w", layer.Digest, err)
	}
	// Consume any trailing blob bytes so the compressed digest covers all of them.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return fmt.Errorf("tart-oci: read layer %s: %w", layer.Digest, err)
	}
	if err := checkDigest("compressed", compHash, layer.Digest); err != nil {
		return err
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

// fetchRawBlob downloads a whole blob to dstPath, verifying its sha256 digest.
func (r *Registry) fetchRawBlob(ctx context.Context, d Descriptor, dstPath string) error {
	body, err := r.blob(ctx, d.Digest)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // read-only body

	out, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("tart-oci: create %s: %w", dstPath, err)
	}
	defer out.Close() //nolint:errcheck // fsync below reports the real write error
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), body); err != nil {
		return fmt.Errorf("tart-oci: download %s: %w", d.Digest, err)
	}
	if err := fsyncFile(out); err != nil {
		return fmt.Errorf("tart-oci: sync %s: %w", dstPath, err)
	}
	return checkDigest(filepath.Base(dstPath), h, d.Digest)
}

// checkDigest verifies that h's sum equals the "sha256:…" digest want.
func checkDigest(what string, h hash.Hash, want string) error {
	got := digestPrefix + hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("tart-oci: %s digest mismatch: got %s, want %s", what, got, want)
	}
	return nil
}
