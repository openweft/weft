package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"time"
)

// bgd holds a decoded block group descriptor.
type bgd struct {
	raw []byte // on-disk bytes (DescSize long)

	BlockBitmapBlock uint64
	InodeBitmapBlock uint64
	InodeTableBlock  uint64
	FreeBlocksCount  uint32
	FreeInodesCount  uint32
	UsedDirsCount    uint32
	Flags            uint16
}

// readBGD reads and decodes the block group descriptor for group g.
func readBGD(f readerWriterAt, fsOffset int64, sb *superblock, g uint32) (*bgd, error) {
	tableBlock := sb.bgdTableBlock()
	descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
	raw := make([]byte, sb.DescSize)
	if _, err := f.ReadAt(raw, fsOffset+descOff); err != nil {
		return nil, fmt.Errorf("ext4: read BGD %d: %w", g, err)
	}
	d := decodeBGD(raw, sb)
	if ext4LockDebug {
		_, file, line, _ := runtime.Caller(1)
		debugPrintf("DEBUG ReadBGD g=%d FreeBlocksCount=%d caller=%s:%d\n", g, d.FreeBlocksCount, file, line)
	}
	return d, nil
}

func decodeBGD(raw []byte, sb *superblock) *bgd {
	le := binary.LittleEndian
	d := &bgd{raw: raw}

	blo := uint64(le.Uint32(raw[0:]))
	ilo := uint64(le.Uint32(raw[4:]))
	tlo := uint64(le.Uint32(raw[8:]))

	if sb.FeatureIncompat&FeatIncompat64bit != 0 && sb.DescSize >= 64 {
		bhi := uint64(le.Uint32(raw[32:]))
		ihi := uint64(le.Uint32(raw[36:]))
		thi := uint64(le.Uint32(raw[40:]))
		d.BlockBitmapBlock = (bhi << 32) | blo
		d.InodeBitmapBlock = (ihi << 32) | ilo
		d.InodeTableBlock = (thi << 32) | tlo
		d.FreeBlocksCount = uint32(le.Uint16(raw[12:])) | (uint32(le.Uint16(raw[44:])) << 16)
		d.FreeInodesCount = uint32(le.Uint16(raw[14:])) | (uint32(le.Uint16(raw[46:])) << 16)
		d.UsedDirsCount = uint32(le.Uint16(raw[16:])) | (uint32(le.Uint16(raw[48:])) << 16)
	} else {
		d.BlockBitmapBlock = blo
		d.InodeBitmapBlock = ilo
		d.InodeTableBlock = tlo
		d.FreeBlocksCount = uint32(le.Uint16(raw[12:]))
		d.FreeInodesCount = uint32(le.Uint16(raw[14:]))
		d.UsedDirsCount = uint32(le.Uint16(raw[16:]))
	}
	d.Flags = le.Uint16(raw[18:])
	return d
}

// encode writes updated free/used counts back into d.raw.
func (d *bgd) encode(sb *superblock) {
	le := binary.LittleEndian
	raw := d.raw

	if sb.FeatureIncompat&FeatIncompat64bit != 0 && sb.DescSize >= 64 {
		le.PutUint16(raw[12:], uint16(d.FreeBlocksCount&0xFFFF))
		le.PutUint16(raw[44:], uint16(d.FreeBlocksCount>>16))
		le.PutUint16(raw[14:], uint16(d.FreeInodesCount&0xFFFF))
		le.PutUint16(raw[46:], uint16(d.FreeInodesCount>>16))
		le.PutUint16(raw[16:], uint16(d.UsedDirsCount&0xFFFF))
		le.PutUint16(raw[48:], uint16(d.UsedDirsCount>>16))
	} else {
		le.PutUint16(raw[12:], uint16(d.FreeBlocksCount))
		le.PutUint16(raw[14:], uint16(d.FreeInodesCount))
		le.PutUint16(raw[16:], uint16(d.UsedDirsCount))
	}
}

// writeBGD writes the updated block group descriptor back to the image and
// refreshes its checksum when metadata_csum is enabled.
func writeBGD(f readerWriterAt, fsOffset int64, sb *superblock, g uint32, d *bgd) error {
	d.encode(sb)

	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		le := binary.LittleEndian
		raw := d.raw
		// Zero the checksum field before computing.
		le.PutUint16(raw[30:], 0)
		gLE := make([]byte, 4)
		le.PutUint32(gLE, g)
		csum := crc32c(sb.csumSeed(), gLE)
		csum = crc32c(csum, raw[:sb.DescSize])
		le.PutUint16(raw[30:], uint16(csum))
	}

	// Serialize preparation of the descriptor update so concurrent
	// writers don't compute overlapping descriptor bytes. Take the per-
	// group descriptor lock while preparing the on-disk bytes, then
	// release it before performing the actual IO to avoid holding a
	// global descriptor lock during potentially long journal fsyncs.
	unlock := lockBGDGroup(f, g)
	tableBlock := sb.bgdTableBlock()
	descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
	// Make a copy of the prepared raw bytes and enqueue them to the
	// commit dispatcher while still holding the BGD lock so ordering is
	// preserved. The worker will apply direct writes (no per-op
	// transactions) to avoid creating isolated journal commits.
	rawCopy := make([]byte, len(d.raw))
	copy(rawCopy, d.raw)
	ops := []commitOp{{startAbs: fsOffset + descOff, data: rawCopy}}
	ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
	if err != nil {
		unlock()
		return fmt.Errorf("ext4: write BGD %d: %w", g, err)
	}
	// Release BGD lock after enqueueing to preserve ordering and avoid
	// holding descriptor locks during IO.
	unlock()
	if ext4LockDebug {
		_, file, line, _ := runtime.Caller(0)
		debugPrintf("DEBUG writeBGD enqueued group=%d seq=%d FreeBlocksCount=%d ts=%d caller=%s:%d\n", g, seq, d.FreeBlocksCount, time.Now().UnixNano(), file, line)
	}
	aerr := <-ack
	if ext4LockDebug {
		_, file, line, _ := runtime.Caller(0)
		debugPrintf("DEBUG ack received writeBGD group=%d seq=%d FreeBlocksCount=%d ack=%p err=%v caller=%s:%d\n", g, seq, d.FreeBlocksCount, &ack, aerr, file, line)
	}
	if aerr != nil {
		return fmt.Errorf("ext4: write BGD %d: %w", g, aerr)
	}
	return nil
}
