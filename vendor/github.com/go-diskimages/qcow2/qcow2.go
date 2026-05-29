package image_qcow2

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Create writes a new empty QCOW2 v2 image at path with the given virtual
// size in bytes. The file contains no allocated data clusters; reads from
// the raw image will return zeros.
func Create(path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("qcow2: size must be positive, got %d", sizeBytes)
	}
	const clusterBits = 16
	const clusterSize = int64(1) << clusterBits // 65536

	l2Entries := clusterSize / 8
	l1Size := (sizeBytes + l2Entries*clusterSize - 1) / (l2Entries * clusterSize)
	if l1Size == 0 {
		l1Size = 1
	}

	// File layout (4 clusters):
	//   cluster 0 (offset      0): QCOW2 header
	//   cluster 1 (offset  65536): L1 table (all zeros = no allocated data)
	//   cluster 2 (offset 131072): refcount table (1 entry → cluster 3)
	//   cluster 3 (offset 196608): refcount block (refcount=1 for clusters 0–3)
	l1Off := clusterSize
	rcTableOff := 2 * clusterSize
	rcBlockOff := 3 * clusterSize

	content := make([]byte, 4*clusterSize)
	writeQCOW2Header(content, sizeBytes, uint32(l1Size), uint64(l1Off), uint64(rcTableOff))
	binary.BigEndian.PutUint64(content[rcTableOff:], uint64(rcBlockOff))
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint16(content[int(rcBlockOff)+i*2:], 1)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create qcow2: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write qcow2: %w", err)
	}
	return nil
}

// writeQCOW2Header fills the first 72 bytes of buf with a valid QCOW2 v2 header.
func writeQCOW2Header(buf []byte, sizeBytes int64, l1Size uint32, l1Off, rcTableOff uint64) {
	copy(buf[0:4], []byte{0x51, 0x46, 0x49, 0xfb}) // magic
	binary.BigEndian.PutUint32(buf[4:8], 2)        // version
	binary.BigEndian.PutUint32(buf[20:24], 16)     // cluster_bits
	binary.BigEndian.PutUint64(buf[24:32], uint64(sizeBytes))
	binary.BigEndian.PutUint32(buf[36:40], l1Size)
	binary.BigEndian.PutUint64(buf[40:48], l1Off)
	binary.BigEndian.PutUint64(buf[48:56], rcTableOff)
	binary.BigEndian.PutUint32(buf[56:60], 1) // refcount_table_clusters
}

// IsQCOW2File returns true if path starts with the QCOW2 magic bytes
// (0x514649fb = "QFI\xfb").
func IsQCOW2File(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	return bytes.Equal(magic, []byte{0x51, 0x46, 0x49, 0xfb})
}

// ConvertToRaw reads a QCOW2 v2/v3 image at src and writes an equivalent
// raw disk image to dst. It writes "N%\n" progress lines to w.
//
// Supported features:
//   - Compressed clusters: deflate (type 0, default)
//
// Unsupported features (return an error):
//   - Encryption (encryption_method != 0 in the header)
//   - ZSTD compression (QCOW2 v3 with compression_type=1)
//
// Backing files are not followed: unallocated clusters are written as zeros.
func ConvertToRaw(src, dst string, w io.Writer) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open qcow2: %w", err)
	}
	defer f.Close()

	// ── Parse header (all big-endian). ───────────────────────────────────────
	hdr := make([]byte, 120)
	n, _ := f.ReadAt(hdr, 0)
	if n < 48 {
		return fmt.Errorf("qcow2 header too short (%d bytes)", n)
	}

	if !bytes.Equal(hdr[0:4], []byte{0x51, 0x46, 0x49, 0xfb}) {
		return fmt.Errorf("not a qcow2 file (bad magic)")
	}
	version := binary.BigEndian.Uint32(hdr[4:8])
	if version < 2 || version > 3 {
		return fmt.Errorf("unsupported qcow2 version %d (only 2 and 3 are supported)", version)
	}

	clusterBits := binary.BigEndian.Uint32(hdr[20:24])
	if clusterBits < 9 || clusterBits > 21 {
		return fmt.Errorf("invalid cluster_bits %d", clusterBits)
	}
	clusterSize := int64(1) << clusterBits

	virtualSize := int64(binary.BigEndian.Uint64(hdr[24:32]))
	if virtualSize <= 0 {
		return fmt.Errorf("invalid virtual size %d", virtualSize)
	}

	encMethod := binary.BigEndian.Uint32(hdr[32:36])
	if encMethod != 0 {
		return fmt.Errorf("encrypted qcow2 images are not supported (encryption_method=%d)", encMethod)
	}

	l1Size := int64(binary.BigEndian.Uint32(hdr[36:40]))
	l1TableOff := int64(binary.BigEndian.Uint64(hdr[40:48]))

	var compressionType byte
	if version == 3 && n >= 105 {
		incompatFeatures := binary.BigEndian.Uint64(hdr[72:80])
		if incompatFeatures&(1<<3) != 0 {
			compressionType = hdr[104]
		}
	}
	if compressionType != 0 {
		return fmt.Errorf("qcow2 compression type %d (zstd) is not supported", compressionType)
	}

	csizeShift := uint64(70 - clusterBits)
	csizeMask := uint64((uint32(1) << (clusterBits - 8)) - 1)
	clusterOffsetMask := (uint64(1) << csizeShift) - 1

	l1Bytes := make([]byte, l1Size*8)
	if _, err := f.ReadAt(l1Bytes, l1TableOff); err != nil {
		return fmt.Errorf("read L1 table: %w", err)
	}
	l1Table := make([]uint64, l1Size)
	for i := range l1Table {
		l1Table[i] = binary.BigEndian.Uint64(l1Bytes[i*8:])
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create raw image: %w", err)
	}
	defer out.Close()

	l2Entries := clusterSize / 8
	totalClusters := (virtualSize + clusterSize - 1) / clusterSize

	zeros := make([]byte, clusterSize)
	dataBuf := make([]byte, clusterSize)
	l2Buf := make([]byte, clusterSize)
	var prevL2Off int64 = -1

	var written int64
	lastPct := -1

	for clusterIdx := int64(0); clusterIdx < totalClusters; clusterIdx++ {
		l1Idx := clusterIdx / l2Entries
		l2Idx := clusterIdx % l2Entries

		size := clusterSize
		if written+size > virtualSize {
			size = virtualSize - written
		}

		l1Entry := l1Table[l1Idx]
		l2Off := int64(l1Entry & 0x00FFFFFFFFFFFE00)

		if l2Off == 0 {
			if _, err := out.Write(zeros[:size]); err != nil {
				return fmt.Errorf("write zeros (cluster %d): %w", clusterIdx, err)
			}
		} else {
			if l2Off != prevL2Off {
				if _, err := f.ReadAt(l2Buf, l2Off); err != nil {
					return fmt.Errorf("read L2 table at %d: %w", l2Off, err)
				}
				prevL2Off = l2Off
			}

			l2Entry := binary.BigEndian.Uint64(l2Buf[l2Idx*8:])

			if l2Entry&(1<<62) != 0 {
				uncompressed, cerr := decompressCluster(f, l2Entry, csizeShift, csizeMask, clusterOffsetMask, clusterSize)
				if cerr != nil {
					return fmt.Errorf("decompress cluster %d: %w", clusterIdx, cerr)
				}
				if _, err := out.Write(uncompressed[:size]); err != nil {
					return fmt.Errorf("write compressed cluster %d: %w", clusterIdx, err)
				}
			} else {
				dataOff := int64(l2Entry & 0x00FFFFFFFFFFFE00)
				if dataOff == 0 {
					if _, err := out.Write(zeros[:size]); err != nil {
						return fmt.Errorf("write zeros (cluster %d): %w", clusterIdx, err)
					}
				} else {
					if _, err := f.ReadAt(dataBuf[:size], dataOff); err != nil {
						return fmt.Errorf("read cluster %d at offset %d: %w", clusterIdx, dataOff, err)
					}
					if _, err := out.Write(dataBuf[:size]); err != nil {
						return fmt.Errorf("write cluster %d: %w", clusterIdx, err)
					}
				}
			}
		}

		written += size
		if pct := int(written * 100 / virtualSize); pct > lastPct {
			lastPct = pct
			_, _ = fmt.Fprintf(w, "%d%%\n", pct)
		}
	}
	return nil
}

func decompressCluster(
	f *os.File,
	l2Entry uint64,
	csizeShift, csizeMask, clusterOffsetMask uint64,
	clusterSize int64,
) ([]byte, error) {
	fileOffset := l2Entry & clusterOffsetMask
	nbCSectors := int64(((l2Entry >> csizeShift) & csizeMask) + 1)

	readStart := int64(fileOffset &^ 511)
	skip := int64(fileOffset & 511)
	readLen := nbCSectors * 512
	rawBuf := make([]byte, readLen)
	if _, err := f.ReadAt(rawBuf, readStart); err != nil {
		return nil, fmt.Errorf("read compressed sectors at %d: %w", readStart, err)
	}

	payload := rawBuf[skip:]
	fr := flate.NewReader(bytes.NewReader(payload))
	defer fr.Close()

	out := make([]byte, clusterSize)
	n, err := io.ReadFull(fr, out)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("deflate decompress: %w", err)
	}
	return out, nil
}
