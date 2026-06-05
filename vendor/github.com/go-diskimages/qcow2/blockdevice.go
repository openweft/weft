package image_qcow2

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
)

// Device provides read-write block device access to a QCOW2 v2/v3 image.
// Unallocated virtual clusters are allocated lazily on first write (COW
// append). Compressed clusters are supported for reading; writing to a
// compressed cluster returns an error (re-compression is not implemented).
type Device struct {
	f           *os.File
	virtualSize int64
	clusterBits uint32
	clusterSize int64
	// Derived mask/shift values for compressed-cluster decoding.
	csizeShift        uint64
	csizeMask         uint64
	clusterOffsetMask uint64
	// L1 table (in-memory copy; updated when L2 tables are allocated).
	l1Table    []uint64
	l1TableOff int64
	// appendOff tracks where the next cluster will be written.
	appendOff int64
	mu        sync.RWMutex
}

// OpenDevice opens the QCOW2 image at path as a read-write block device.
func OpenDevice(path string) (*Device, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("qcow2: open %s: %w", path, err)
	}
	d, err := newDevice(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return d, nil
}

func newDevice(f *os.File) (*Device, error) {
	hdr := make([]byte, 120)
	n, _ := f.ReadAt(hdr, 0)
	if n < 48 {
		return nil, fmt.Errorf("qcow2 header too short (%d bytes)", n)
	}
	if hdr[0] != 0x51 || hdr[1] != 0x46 || hdr[2] != 0x49 || hdr[3] != 0xfb {
		return nil, fmt.Errorf("not a qcow2 file (bad magic)")
	}
	version := binary.BigEndian.Uint32(hdr[4:8])
	if version < 2 || version > 3 {
		return nil, fmt.Errorf("unsupported qcow2 version %d", version)
	}
	if enc := binary.BigEndian.Uint32(hdr[32:36]); enc != 0 {
		return nil, fmt.Errorf("encrypted qcow2 not supported")
	}
	clusterBits := binary.BigEndian.Uint32(hdr[20:24])
	if clusterBits < 9 || clusterBits > 21 {
		return nil, fmt.Errorf("invalid cluster_bits %d", clusterBits)
	}
	virtualSize := int64(binary.BigEndian.Uint64(hdr[24:32]))
	if virtualSize <= 0 {
		return nil, fmt.Errorf("invalid virtual size %d", virtualSize)
	}
	l1Size := int64(binary.BigEndian.Uint32(hdr[36:40]))
	l1TableOff := int64(binary.BigEndian.Uint64(hdr[40:48]))
	l1Table, err := loadL1Table(f, l1TableOff, l1Size)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("qcow2: stat: %w", err)
	}
	clusterSize := int64(1) << clusterBits
	// Round file size up to the next cluster boundary for safe appending.
	fileSize := fi.Size()
	appendOff := (fileSize + clusterSize - 1) &^ (clusterSize - 1)
	if appendOff < fileSize {
		appendOff = fileSize
	}
	return &Device{
		f:                 f,
		virtualSize:       virtualSize,
		clusterBits:       clusterBits,
		clusterSize:       clusterSize,
		csizeShift:        uint64(70 - clusterBits),
		csizeMask:         uint64((uint32(1) << (clusterBits - 8)) - 1),
		clusterOffsetMask: (uint64(1) << uint64(70-clusterBits)) - 1,
		l1Table:           l1Table,
		l1TableOff:        l1TableOff,
		appendOff:         appendOff,
	}, nil
}

func loadL1Table(f *os.File, off, size int64) ([]uint64, error) {
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size*8)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("read L1 table: %w", err)
	}
	tbl := make([]uint64, size)
	for i := range tbl {
		tbl[i] = binary.BigEndian.Uint64(buf[i*8:])
	}
	return tbl, nil
}

// ReadAt implements io.ReaderAt for the virtual disk address space.
func (d *Device) ReadAt(p []byte, off int64) (int, error) {
	if off >= d.virtualSize {
		return 0, io.EOF
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.readAtLocked(p, off)
}

func (d *Device) readAtLocked(p []byte, off int64) (int, error) {
	total := 0
	for len(p) > 0 && off < d.virtualSize {
		n, err := d.readOneChunk(p, off)
		total += n
		p = p[n:]
		off += int64(n)
		if err != nil {
			return total, err
		}
	}
	if len(p) > 0 {
		return total, io.EOF
	}
	return total, nil
}

func (d *Device) readOneChunk(p []byte, off int64) (int, error) {
	clusterIdx := off >> d.clusterBits
	clusterOff := off & (d.clusterSize - 1)
	avail := d.clusterSize - clusterOff
	if avail > int64(len(p)) {
		avail = int64(len(p))
	}
	if off+avail > d.virtualSize {
		avail = d.virtualSize - off
	}
	l2Entry, err := d.readL2Entry(clusterIdx)
	if err != nil {
		return 0, err
	}
	if l2Entry&(1<<62) != 0 {
		return d.readCompressedChunk(p[:avail], clusterOff, l2Entry)
	}
	physOff := int64(l2Entry & 0x00FFFFFFFFFFFE00)
	if physOff == 0 {
		for i := range p[:avail] {
			p[i] = 0
		}
		return int(avail), nil
	}
	n, err := d.f.ReadAt(p[:avail], physOff+clusterOff)
	return n, err
}

func (d *Device) readCompressedChunk(dst []byte, clusterOff int64, l2Entry uint64) (int, error) {
	data, err := decompressCluster(d.f, l2Entry, d.csizeShift, d.csizeMask, d.clusterOffsetMask, d.clusterSize)
	if err != nil {
		return 0, err
	}
	n := copy(dst, data[clusterOff:])
	return n, nil
}

func (d *Device) readL2Entry(clusterIdx int64) (uint64, error) {
	l2Entries := d.clusterSize / 8
	l1Idx := clusterIdx / l2Entries
	l2Idx := clusterIdx % l2Entries
	if l1Idx >= int64(len(d.l1Table)) {
		return 0, nil
	}
	l2Off := int64(d.l1Table[l1Idx] & 0x00FFFFFFFFFFFE00)
	if l2Off == 0 {
		return 0, nil
	}
	buf := make([]byte, 8)
	if _, err := d.f.ReadAt(buf, l2Off+l2Idx*8); err != nil {
		return 0, fmt.Errorf("read L2 entry (cluster %d): %w", clusterIdx, err)
	}
	return binary.BigEndian.Uint64(buf), nil
}

// WriteAt implements io.WriterAt for the virtual disk address space.
// Compressed clusters cannot be overwritten; use uncompressed images for
// read-write access.
func (d *Device) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= d.virtualSize {
		return 0, fmt.Errorf("qcow2: write at %d outside virtual size %d", off, d.virtualSize)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	total := 0
	for len(p) > 0 {
		n, err := d.writeOneChunk(p, off)
		total += n
		p = p[n:]
		off += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (d *Device) writeOneChunk(p []byte, off int64) (int, error) {
	clusterIdx := off >> d.clusterBits
	clusterOff := off & (d.clusterSize - 1)
	avail := d.clusterSize - clusterOff
	if avail > int64(len(p)) {
		avail = int64(len(p))
	}
	physOff, err := d.ensureCluster(clusterIdx)
	if err != nil {
		return 0, err
	}
	n, err := d.f.WriteAt(p[:avail], physOff+clusterOff)
	return n, err
}

// ensureCluster returns the physical offset of the cluster for clusterIdx,
// allocating L2 tables and data clusters as needed.
func (d *Device) ensureCluster(clusterIdx int64) (int64, error) {
	l2Entries := d.clusterSize / 8
	l1Idx := clusterIdx / l2Entries
	l2Idx := clusterIdx % l2Entries
	if l1Idx >= int64(len(d.l1Table)) {
		return 0, fmt.Errorf("qcow2: cluster index %d beyond L1 range", clusterIdx)
	}
	l2Off, err := d.ensureL2(l1Idx)
	if err != nil {
		return 0, err
	}
	return d.ensureDataCluster(l2Off, l2Idx)
}

func (d *Device) ensureL2(l1Idx int64) (int64, error) {
	l2Off := int64(d.l1Table[l1Idx] & 0x00FFFFFFFFFFFE00)
	if l2Off != 0 {
		return l2Off, nil
	}
	// Allocate a new zeroed cluster to hold the L2 table.
	l2Off, err := d.allocCluster()
	if err != nil {
		return 0, fmt.Errorf("allocate L2 table: %w", err)
	}
	// Persist the new L1 entry.
	entry := make([]byte, 8)
	binary.BigEndian.PutUint64(entry, uint64(l2Off))
	if _, err := d.f.WriteAt(entry, d.l1TableOff+l1Idx*8); err != nil {
		return 0, fmt.Errorf("write L1 entry: %w", err)
	}
	d.l1Table[l1Idx] = uint64(l2Off)
	return l2Off, nil
}

func (d *Device) ensureDataCluster(l2Off, l2Idx int64) (int64, error) {
	buf := make([]byte, 8)
	if _, err := d.f.ReadAt(buf, l2Off+l2Idx*8); err != nil {
		return 0, fmt.Errorf("read L2 entry: %w", err)
	}
	l2Entry := binary.BigEndian.Uint64(buf)
	if l2Entry&(1<<62) != 0 {
		return 0, fmt.Errorf("qcow2: cannot write to compressed cluster")
	}
	physOff := int64(l2Entry & 0x00FFFFFFFFFFFE00)
	if physOff != 0 {
		return physOff, nil
	}
	// Allocate and link a new data cluster.
	physOff, err := d.allocCluster()
	if err != nil {
		return 0, fmt.Errorf("allocate data cluster: %w", err)
	}
	binary.BigEndian.PutUint64(buf, uint64(physOff))
	if _, err := d.f.WriteAt(buf, l2Off+l2Idx*8); err != nil {
		return 0, fmt.Errorf("write L2 entry: %w", err)
	}
	return physOff, nil
}

// allocCluster extends the file by one cluster and returns its offset.
// The new cluster is implicitly zeroed by the OS (file hole / truncate).
func (d *Device) allocCluster() (int64, error) {
	off := (d.appendOff + d.clusterSize - 1) &^ (d.clusterSize - 1)
	if err := d.f.Truncate(off + d.clusterSize); err != nil {
		return 0, fmt.Errorf("extend file for cluster at %d: %w", off, err)
	}
	d.appendOff = off + d.clusterSize
	return off, nil
}

// Sync flushes all writes to the underlying file.
func (d *Device) Sync() error { return d.f.Sync() }

// Size returns the virtual size of the QCOW2 image.
func (d *Device) Size() (int64, error) { return d.virtualSize, nil }

// Fd returns the underlying file descriptor. Useful for callers that need to
// pass the QCOW2 container to syscall- or sparse-tools-flavoured APIs (note
// the fd is the container, not the virtual disk — random reads via the fd
// yield QCOW2-format bytes, not raw guest data; use ReadAt for the latter).
func (d *Device) Fd() uintptr { return d.f.Fd() }

// Truncate is not supported; the virtual size is fixed at image creation.
func (d *Device) Truncate(size int64) error {
	return fmt.Errorf("qcow2: Truncate not supported (virtual size is fixed)")
}

// Close closes the underlying file.
func (d *Device) Close() error { return d.f.Close() }
