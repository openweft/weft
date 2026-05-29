package image_qcow2

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Format is the QCOW2 disk image format.
// A Format value satisfies the diskimage_format.Format interface defined in
// github.com/go-diskimages/interface without requiring an import of that
// module (Go structural typing).
type Format struct{}

// Name returns "qcow2".
func (Format) Name() string { return "qcow2" }

// Create creates a new QCOW2 disk image. Delegates to the package-level Create.
func (Format) Create(path string, sizeBytes int64) error {
	return Create(path, sizeBytes)
}

// Detect returns (true, nil) if path contains a QCOW2 image.
func (Format) Detect(path string) (bool, error) {
	return IsQCOW2File(path), nil
}

// ToRaw converts the QCOW2 image at src to a raw disk image at dst.
// Progress messages are written to w.
func (Format) ToRaw(src, dst string, w io.Writer) error {
	return ConvertToRaw(src, dst, w)
}

// Resize changes the virtual size of the QCOW2 image at path. Only growing is
// supported; shrinking returns an error because it may silently discard data.
// The L1 table is extended in-place when the new size requires more entries.
func (Format) Resize(path string, newSizeBytes int64) error {
	if newSizeBytes <= 0 {
		return fmt.Errorf("qcow2: Resize: size must be positive, got %d", newSizeBytes)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("qcow2: Resize: open: %w", err)
	}
	defer f.Close()
	hdr, curSize, clusterBits, l1TableOff, err := readQCOW2ResizeHeader(f)
	if err != nil {
		return fmt.Errorf("qcow2: Resize: %w", err)
	}
	if newSizeBytes < curSize {
		return fmt.Errorf("qcow2: Resize: shrinking not supported (cur=%d new=%d)", curSize, newSizeBytes)
	}
	if newSizeBytes == curSize {
		return nil
	}
	return applyQCOW2Resize(f, hdr, newSizeBytes, clusterBits, l1TableOff)
}

// readQCOW2ResizeHeader reads fields needed for resize from a QCOW2 header.
func readQCOW2ResizeHeader(f *os.File) (hdr []byte, virtualSize int64, clusterBits uint32, l1TableOff int64, err error) {
	buf := make([]byte, 120)
	n, _ := f.ReadAt(buf, 0)
	if n < 48 {
		return nil, 0, 0, 0, fmt.Errorf("header too short (%d bytes)", n)
	}
	magic := []byte{0x51, 0x46, 0x49, 0xfb}
	for i, b := range magic {
		if buf[i] != b {
			return nil, 0, 0, 0, fmt.Errorf("not a qcow2 file (bad magic)")
		}
	}
	cb := binary.BigEndian.Uint32(buf[20:24])
	if cb < 9 || cb > 21 {
		return nil, 0, 0, 0, fmt.Errorf("invalid cluster_bits %d", cb)
	}
	vs := int64(binary.BigEndian.Uint64(buf[24:32]))
	l1Off := int64(binary.BigEndian.Uint64(buf[40:48]))
	return buf[:n], vs, cb, l1Off, nil
}

// applyQCOW2Resize writes the new virtual size into the header and extends
// the L1 table on disk when necessary.
func applyQCOW2Resize(f *os.File, hdr []byte, newSizeBytes int64, clusterBits uint32, l1TableOff int64) error {
	clusterSize := int64(1) << clusterBits
	l2Entries := clusterSize / 8
	oldL1Size := int64(binary.BigEndian.Uint32(hdr[36:40]))
	newL1Size := (newSizeBytes + l2Entries*clusterSize - 1) / (l2Entries * clusterSize)
	if newL1Size < 1 {
		newL1Size = 1
	}
	// Write the new virtual size into the header at offset 24.
	sizeBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(sizeBuf, uint64(newSizeBytes))
	if _, err := f.WriteAt(sizeBuf, 24); err != nil {
		return fmt.Errorf("qcow2: Resize: write virtual size: %w", err)
	}
	if newL1Size <= oldL1Size {
		return nil
	}
	return extendL1Table(f, l1TableOff, oldL1Size, newL1Size, clusterBits)
}

// extendL1Table appends zero entries to the L1 table to cover newL1Size entries.
func extendL1Table(f *os.File, l1TableOff, oldL1Size, newL1Size int64, clusterBits uint32) error {
	// Update the L1 size field in the header at offset 36.
	l1SizeBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(l1SizeBuf, uint32(newL1Size))
	if _, err := f.WriteAt(l1SizeBuf, 36); err != nil {
		return fmt.Errorf("qcow2: Resize: write L1 size: %w", err)
	}
	// Zero-fill the new L1 entries (each is 8 bytes; QCOW2 zero = unallocated).
	newEntries := newL1Size - oldL1Size
	zeros := make([]byte, newEntries*8)
	if _, err := f.WriteAt(zeros, l1TableOff+oldL1Size*8); err != nil {
		return fmt.Errorf("qcow2: Resize: extend L1 table: %w", err)
	}
	return nil
}
