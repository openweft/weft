package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
)

// superblockSize is the on-disk size of the ext4 superblock.
const superblockSize = 1024

// superblock holds the fields we actually use from the ext4 superblock.
type superblock struct {
	raw []byte // full 1024-byte on-disk image

	// Decoded fields
	InodesCount      uint32
	BlocksCount      uint64 // hi<<32 | lo
	FreeBlocksCount  uint64
	FreeInodesCount  uint32
	FirstDataBlock   uint32
	BlockSize        uint32 // 1024 << s_log_block_size
	BlocksPerGroup   uint32
	InodesPerGroup   uint32
	Magic            uint16
	FeatureIncompat  uint32
	FeatureROCompat  uint32
	UUID             [16]byte
	Label            string // s_volume_name, null-stripped (max 16 bytes on disk)
	InodeSize        uint16
	DescSize         uint16 // block group descriptor size (64-bit mode)
	LogGroupsPerFlex uint8  // groups per flex group (flex_bg)
	ChecksumSeed     uint32 // pre-computed or derived from UUID
	ChecksumType     uint8  // 1 = CRC32c
}

// readSuperblock reads and decodes the ext4 superblock. The superblock always
// resides at byte offset 1024 within the filesystem (regardless of block size).
func readSuperblock(f readerWriterAt, fsOffset int64) (*superblock, error) {
	raw := make([]byte, superblockSize)
	if _, err := f.ReadAt(raw, fsOffset+1024); err != nil {
		return nil, fmt.Errorf("ext4: read superblock: %w", err)
	}
	sb := &superblock{raw: raw}
	le := binary.LittleEndian

	sb.Magic = le.Uint16(raw[56:])
	if sb.Magic != 0xEF53 {
		return nil, fmt.Errorf("ext4: bad superblock magic 0x%04X", sb.Magic)
	}

	sb.InodesCount = le.Uint32(raw[0:])
	lo := uint64(le.Uint32(raw[4:]))
	hi := uint64(le.Uint32(raw[0x150:]))
	sb.BlocksCount = (hi << 32) | lo

	flo := uint64(le.Uint32(raw[12:]))
	fhi := uint64(le.Uint32(raw[0x158:]))
	sb.FreeBlocksCount = (fhi << 32) | flo

	sb.FreeInodesCount = le.Uint32(raw[16:])
	sb.FirstDataBlock = le.Uint32(raw[20:])
	sb.BlockSize = 1024 << le.Uint32(raw[24:])
	sb.BlocksPerGroup = le.Uint32(raw[32:])
	sb.InodesPerGroup = le.Uint32(raw[40:])
	sb.FeatureIncompat = le.Uint32(raw[96:])
	sb.FeatureROCompat = le.Uint32(raw[100:])
	copy(sb.UUID[:], raw[104:120])
	// s_volume_name is 16 bytes of plain ASCII/UTF-8, null-padded.
	// Strip the padding so callers get a usable string.
	sb.Label = decodeNullPadded(raw[120:136])
	sb.InodeSize = le.Uint16(raw[88:])
	if sb.InodeSize == 0 {
		sb.InodeSize = 128
	}
	sb.DescSize = le.Uint16(raw[0xFE:])
	if sb.DescSize == 0 || sb.DescSize < 32 {
		sb.DescSize = 32
	}
	sb.LogGroupsPerFlex = raw[0x174]
	sb.ChecksumType = raw[0x175]
	sb.ChecksumSeed = le.Uint32(raw[0x270:])

	return sb, nil
}

// writeSuperblock updates free counts in the raw superblock bytes and writes
// it back to the image.
func writeSuperblock(f readerWriterAt, fsOffset int64, sb *superblock) error {
	le := binary.LittleEndian
	raw := sb.raw

	// Update total blocks count (lo/hi) so on-disk superblock reflects Grow().
	le.PutUint32(raw[4:], uint32(sb.BlocksCount&0xFFFFFFFF))
	le.PutUint32(raw[0x150:], uint32(sb.BlocksCount>>32))

	// Update free blocks (split into lo/hi).
	le.PutUint32(raw[12:], uint32(sb.FreeBlocksCount&0xFFFFFFFF))
	le.PutUint32(raw[0x158:], uint32(sb.FreeBlocksCount>>32))
	le.PutUint32(raw[16:], sb.FreeInodesCount)

	// Recompute superblock checksum (at offset 0x3FC) using the filesystem
	// checksum seed per ext4 metadata_csum semantics.
	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		le.PutUint32(raw[0x3FC:], 0)
		seed := sb.csumSeed()
		csum := crc32c(seed, raw[:0x3FC])
		le.PutUint32(raw[0x3FC:], csum)
	}

	// Serialize preparation of the superblock update with the per-block
	// lock for the block containing the superblock, but release the lock
	// before performing IO so that journal fsync/write-through doesn't hold
	// the descriptor lock.
	bs := int64(sb.BlockSize)
	sbBlock := uint64(1024 / bs)
	// Acquire the BGD table block lock while preparing the superblock bytes
	// and then enqueue via the commit dispatcher to avoid creating a local
	// journal transaction here. Use group 0 for superblock writes so the
	// dispatcher preserves ordering with other group-specific updates.
	unlock := lockBGDTableBlock(f, sbBlock)
	rawCopy := make([]byte, len(raw))
	copy(rawCopy, raw)
	ops := []commitOp{{startAbs: fsOffset + 1024, data: rawCopy}}
	ack, _, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, 0, ops)
	if err != nil {
		unlock()
		return fmt.Errorf("ext4: write superblock: %w", err)
	}
	unlock()
	aerr := <-ack
	if ext4LockDebug {
		_, file, line, _ := runtime.Caller(0)
		debugPrintf("DEBUG ack received writeSuperblock group=0 ack=%p err=%v caller=%s:%d\n", &ack, aerr, file, line)
	}
	if aerr != nil {
		return fmt.Errorf("ext4: write superblock: %w", aerr)
	}
	return nil
}

// numBlockGroups returns how many block groups the filesystem has.
func (sb *superblock) numBlockGroups() uint32 {
	return uint32((sb.BlocksCount + uint64(sb.BlocksPerGroup) - 1) / uint64(sb.BlocksPerGroup))
}

// bgdTableBlock returns the block number of the block group descriptor table.
// For block sizes > 1024, the BGD table starts at block 1 (=the block holding
// the superblock). For 1024-byte blocks, it starts at block 2.
func (sb *superblock) bgdTableBlock() uint64 {
	if sb.BlockSize > 1024 {
		return 1
	}
	return 2
}

// csumSeed returns the CRC32c seed used for all metadata checksums.
// When FeatIncompatCsumSeed is set the seed is stored in the superblock;
// otherwise it is derived from the filesystem UUID.
func (sb *superblock) csumSeed() uint32 {
	if sb.FeatureIncompat&FeatIncompatCsumSeed != 0 {
		return sb.ChecksumSeed
	}
	return crc32c(^uint32(0), sb.UUID[:])
}

// encodeRaw updates sb.raw from the decoded fields and recomputes the
// superblock checksum when metadata_csum is enabled. This mirrors the
// behaviour performed by writeSuperblock but does not perform any I/O.
func (sb *superblock) encodeRaw() {
	le := binary.LittleEndian
	raw := sb.raw

	// Use atomic loads for fields that may be updated concurrently.
	bc := atomic.LoadUint64(&sb.BlocksCount)
	le.PutUint32(raw[4:], uint32(bc&0xFFFFFFFF))
	le.PutUint32(raw[0x150:], uint32(bc>>32))

	fbc := atomic.LoadUint64(&sb.FreeBlocksCount)
	le.PutUint32(raw[12:], uint32(fbc&0xFFFFFFFF))
	le.PutUint32(raw[0x158:], uint32(fbc>>32))
	fic := atomic.LoadUint32(&sb.FreeInodesCount)
	le.PutUint32(raw[16:], fic)

	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		le.PutUint32(raw[0x3FC:], 0)
		seed := sb.csumSeed()
		csum := crc32c(seed, raw[:0x3FC])
		le.PutUint32(raw[0x3FC:], csum)
	}
}
