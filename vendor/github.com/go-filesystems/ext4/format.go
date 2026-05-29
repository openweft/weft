package filesystem_ext4

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// Layout constants used by Format.
const (
	fmtBlockSize      = 4096
	fmtLogBlockSize   = 2 // 1024 << 2 = 4096
	fmtInodeSize      = 256
	fmtInodesPerGroup = 256

	fmtBlocksPerGroup   = 32768
	fmtDescSize         = 32
	fmtReservedInodes   = 10
	fmtInodeTableBlocks = fmtInodesPerGroup * fmtInodeSize / fmtBlockSize // 16
)

// FormatConfig holds optional parameters for Format.
// All fields are optional; sensible defaults are used when left at their zero value.
type FormatConfig struct {
	// UUID is the filesystem UUID. A random v4 UUID is generated when all bytes are zero.
	UUID [16]byte
	// Label is the volume label stored in the superblock (trimmed to 16 bytes).
	Label string
}

type formatFile interface {
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Close() error
}

var formatOpenFile = func(path string) (formatFile, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
}

var formatRandRead = func(p []byte) (int, error) {
	return rand.Read(p)
}

var formatOpenFS = Open

// formatWriteAt wraps writes performed during Format. If the provided
// file also implements `readerWriterAt`, route through `writeAtWithTrace`
// so test builds can trace and acquire any necessary inode-table locks.
func formatWriteAt(f formatFile, fsOffset int64, sb *superblock, off int64, data []byte) (int, error) {
	if rwa, ok := f.(readerWriterAt); ok {
		return writeAtWithTrace(rwa, fsOffset, sb, off, data)
	}
	return f.WriteAt(data, off)
}

// Format creates a new ext4 filesystem in the file at path.
// The file is created (or truncated) and formatted. sizeBytes must be a
// multiple of 4096 and large enough to hold filesystem metadata plus at
// least one data block.
//
// On success the newly formatted filesystem is opened and returned; the
// caller must Close it when done.
func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error) {
	const minBlocks = fmtInodeTableBlocks + 5 // SB + BGDT + 2 bitmaps + inode table + 1 data
	if sizeBytes%fmtBlockSize != 0 {
		return nil, fmt.Errorf("ext4: format: size %d is not a multiple of %d", sizeBytes, fmtBlockSize)
	}
	if sizeBytes < int64(minBlocks)*fmtBlockSize {
		return nil, fmt.Errorf("ext4: format: size %d too small (minimum %d bytes)", sizeBytes, int64(minBlocks)*fmtBlockSize)
	}
	totalBlocks64 := uint64(sizeBytes / fmtBlockSize)
	if totalBlocks64 > uint64(^uint32(0)) {
		return nil, fmt.Errorf("ext4: format: size %d too large", sizeBytes)
	}

	// Create/truncate the backing file.
	f, err := formatOpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("ext4: format: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("ext4: format: truncate: %w", err)
	}

	// Generate a random UUID v4 when none provided.
	uuid := cfg.UUID
	if uuid == [16]byte{} {
		if _, err := formatRandRead(uuid[:]); err != nil {
			f.Close()
			return nil, fmt.Errorf("ext4: format: generate UUID: %w", err)
		}
		uuid[6] = (uuid[6] & 0x0F) | 0x40 // version 4
		uuid[8] = (uuid[8] & 0x3F) | 0x80 // variant 1 (RFC 4122)
	}

	le := binary.LittleEndian
	now := uint32(time.Now().Unix())

	totalBlocks := uint32(totalBlocks64)
	nGroups := (totalBlocks + fmtBlocksPerGroup - 1) / fmtBlocksPerGroup
	bgdtBlocks := (nGroups*fmtDescSize + fmtBlockSize - 1) / fmtBlockSize

	// ── Group-0 layout ───────────────────────────────────────────────────────
	// Block 0                : superblock area (SB at byte 1024 within block 0)
	// Blocks 1..bgdtBlocks   : Block Group Descriptor Table
	// Block bgdtBlocks+1     : block bitmap for group 0
	// Block bgdtBlocks+2     : inode bitmap for group 0
	// Blocks bgdtBlocks+3..  : inode table for group 0 (fmtInodeTableBlocks blocks)
	// Block bgdtBlocks+3+fmtInodeTableBlocks : root directory data block

	g0BitmapBlock := uint32(1 + bgdtBlocks)
	g0InodeBmap := g0BitmapBlock + 1
	g0InodeTable := g0BitmapBlock + 2
	g0RootDirDataBlock := g0InodeTable + fmtInodeTableBlocks

	// ── Group-g (g>0) layout ─────────────────────────────────────────────────
	// Block g*GPG+0 : block bitmap
	// Block g*GPG+1 : inode bitmap
	// Blocks g*GPG+2..g*GPG+1+fmtInodeTableBlocks : inode table
	// Block g*GPG+2+fmtInodeTableBlocks : first free data block

	gMeta := uint32(2 + fmtInodeTableBlocks) // metadata blocks per non-zero group

	// Compute totals.
	totalInodes := fmtInodesPerGroup * nGroups
	totalFreeBlocks := uint64(0)
	for g := uint32(0); g < nGroups; g++ {
		groupBlocks := uint32(fmtBlocksPerGroup)
		if g == nGroups-1 {
			groupBlocks = totalBlocks - g*fmtBlocksPerGroup
		}
		meta := gMeta
		if g == 0 {
			meta = g0RootDirDataBlock + 1 // every block up to and including the root dir data block
		}
		if groupBlocks > meta {
			totalFreeBlocks += uint64(groupBlocks - meta)
		}
	}
	// Reserved inodes 1-10 + root (inode 2 is one of those 10).
	totalFreeInodes := totalInodes - fmtReservedInodes

	// ── 1. Superblock (1024 bytes at byte offset 1024) ───────────────────────
	raw := make([]byte, 1024)
	le.PutUint32(raw[0:], totalInodes)
	le.PutUint32(raw[4:], uint32(totalBlocks&0xFFFFFFFF)) // blocks_count_lo
	// raw[8:12] = r_blocks_count_lo = 0 (no reserved blocks)
	le.PutUint32(raw[12:], uint32(totalFreeBlocks&0xFFFFFFFF))
	le.PutUint32(raw[16:], totalFreeInodes)
	// raw[20:24] = first_data_block = 0 (block size > 1024)
	le.PutUint32(raw[24:], fmtLogBlockSize) // log_block_size
	le.PutUint32(raw[28:], fmtLogBlockSize) // log_cluster_size
	le.PutUint32(raw[32:], fmtBlocksPerGroup)
	le.PutUint32(raw[36:], fmtBlocksPerGroup) // clusters_per_group
	le.PutUint32(raw[40:], fmtInodesPerGroup)
	le.PutUint32(raw[44:], now) // s_mtime
	le.PutUint32(raw[48:], now) // s_wtime
	// raw[52:54] = mnt_count = 0
	le.PutUint16(raw[54:], 20)     // max_mnt_count
	le.PutUint16(raw[56:], 0xEF53) // magic
	le.PutUint16(raw[58:], 1)      // state: clean (EXT2_VALID_FS)
	le.PutUint16(raw[60:], 1)      // errors: continue
	le.PutUint32(raw[64:], now)    // lastcheck
	// raw[68:72] = checkinterval = 0
	// raw[72:76] = creator_os = 0 (Linux)
	le.PutUint32(raw[76:], 1)  // rev_level: 1 (EXT2_DYNAMIC_REV)
	le.PutUint32(raw[84:], 11) // first_ino: 11
	le.PutUint16(raw[88:], fmtInodeSize)
	// raw[90:92] = block_group_nr = 0 (primary SB)
	// raw[92:96] = feature_compat = 0
	le.PutUint32(raw[96:], FeatIncompatFiletype|FeatIncompatExtents) // feature_incompat
	// raw[100:104] = feature_ro_compat = 0
	copy(raw[104:], uuid[:])
	label := cfg.Label
	if len(label) > 16 {
		label = label[:16]
	}
	copy(raw[120:], label)
	le.PutUint16(raw[0xFE:], fmtDescSize) // s_desc_size
	// raw[0x174] = log_groups_per_flex = 0 (no flex_bg)
	// raw[0x175] = checksum_type = 0 (no metadata_csum)
	// raw[0x270:0x274] = checksum_seed = 0 (will be derived from UUID)
	if _, err := formatWriteAt(f, 0, nil, 1024, raw); err != nil {
		f.Close()
		return nil, fmt.Errorf("ext4: format: write superblock: %w", err)
	}

	// ── 2. Block Group Descriptor Table (block 1, byte offset 4096) ──────────
	bgdtBuf := make([]byte, int(nGroups)*fmtDescSize)
	for g := uint32(0); g < nGroups; g++ {
		groupBlockStart := uint64(g) * fmtBlocksPerGroup
		groupBlocks := uint32(fmtBlocksPerGroup)
		if g == nGroups-1 {
			groupBlocks = totalBlocks - g*fmtBlocksPerGroup
		}

		var bitmapBlock, inodeBmapBlock, inodeTableBlock uint64
		var freeBlocks, freeInodes, usedDirs uint16

		if g == 0 {
			bitmapBlock = uint64(g0BitmapBlock)
			inodeBmapBlock = uint64(g0InodeBmap)
			inodeTableBlock = uint64(g0InodeTable)
			used := uint32(g0RootDirDataBlock + 1)
			if groupBlocks >= used {
				freeBlocks = uint16(groupBlocks - used)
			}
			freeInodes = uint16(fmtInodesPerGroup - fmtReservedInodes)
			usedDirs = 1
		} else {
			bitmapBlock = groupBlockStart
			inodeBmapBlock = groupBlockStart + 1
			inodeTableBlock = groupBlockStart + 2
			if groupBlocks >= gMeta {
				freeBlocks = uint16(groupBlocks - gMeta)
			}
			freeInodes = uint16(fmtInodesPerGroup)
			usedDirs = 0
		}

		d := bgdtBuf[int(g)*fmtDescSize:]
		le.PutUint32(d[0:], uint32(bitmapBlock))
		le.PutUint32(d[4:], uint32(inodeBmapBlock))
		le.PutUint32(d[8:], uint32(inodeTableBlock))
		le.PutUint16(d[12:], freeBlocks)
		le.PutUint16(d[14:], freeInodes)
		le.PutUint16(d[16:], usedDirs)
	}
	if _, err := formatWriteAt(f, 0, nil, fmtBlockSize, bgdtBuf); err != nil {
		f.Close()
		return nil, fmt.Errorf("ext4: format: write BGDT: %w", err)
	}

	// ── 3. Block bitmaps, inode bitmaps, and inode tables ────────────────────
	for g := uint32(0); g < nGroups; g++ {
		groupBlockStart := uint64(g) * fmtBlocksPerGroup
		groupBlocks := uint32(fmtBlocksPerGroup)
		if g == nGroups-1 {
			groupBlocks = totalBlocks - g*fmtBlocksPerGroup
		}

		var bitmapOff, inodeBmapOff int64

		// Block bitmap.
		bmap := make([]byte, fmtBlockSize)
		if g == 0 {
			// Mark blocks 0..g0RootDirDataBlock as used.
			for i := uint32(0); i <= g0RootDirDataBlock; i++ {
				setBit(bmap, int(i))
			}
			bitmapOff = int64(g0BitmapBlock) * fmtBlockSize
		} else {
			// Mark metadata blocks (bitmaps + inode table) as used.
			for i := uint32(0); i < gMeta; i++ {
				setBit(bmap, int(i))
			}
			bitmapOff = int64(groupBlockStart) * fmtBlockSize
		}
		// Mark bits beyond actual group size as unavailable.
		for i := groupBlocks; i < fmtBlocksPerGroup; i++ {
			setBit(bmap, int(i))
		}
		if _, err := formatWriteAt(f, 0, nil, bitmapOff, bmap); err != nil {
			f.Close()
			return nil, fmt.Errorf("ext4: format: write block bitmap (group %d): %w", g, err)
		}

		// Inode bitmap.
		ibmap := make([]byte, fmtBlockSize)
		if g == 0 {
			// Mark inodes 1-10 (bits 0-9) as reserved.
			for i := 0; i < fmtReservedInodes; i++ {
				setBit(ibmap, i)
			}
			inodeBmapOff = int64(g0InodeBmap) * fmtBlockSize
		} else {
			inodeBmapOff = int64(groupBlockStart+1) * fmtBlockSize
		}
		if _, err := formatWriteAt(f, 0, nil, inodeBmapOff, ibmap); err != nil {
			f.Close()
			return nil, fmt.Errorf("ext4: format: write inode bitmap (group %d): %w", g, err)
		}
		// Inode table blocks are zeroed by the sparse file — no explicit write needed.
	}

	// ── 4. Root directory inode (inode 2, group 0, local index 1) ────────────
	// Off = InodeTableBlock * blockSize + localIdx * InodeSize = g0InodeTable*4096 + 1*256.
	rootInodeOff := int64(g0InodeTable)*fmtBlockSize + int64(1)*fmtInodeSize
	rootRaw := make([]byte, fmtInodeSize)
	le.PutUint16(rootRaw[inodeOffMode:], 0x41ED)  // directory + rwxr-xr-x
	le.PutUint16(rootRaw[inodeOffLinksCount:], 2) // "." and ".."
	le.PutUint32(rootRaw[inodeOffSizeLo:], fmtBlockSize)
	le.PutUint32(rootRaw[28:], fmtBlockSize/512) // i_blocks_lo (512-byte sectors)
	le.PutUint32(rootRaw[8:], now)               // i_atime
	le.PutUint32(rootRaw[12:], now)              // i_ctime
	le.PutUint32(rootRaw[16:], now)              // i_mtime
	le.PutUint32(rootRaw[inodeOffFlags:], InodeFlagExtents)
	// Inline extent tree in i_block (60 bytes at offset 40).
	extBuf := rootRaw[inodeOffBlock : inodeOffBlock+60]
	le.PutUint16(extBuf[0:], ExtentMagic) // eh_magic
	le.PutUint16(extBuf[2:], 1)           // eh_entries = 1
	le.PutUint16(extBuf[4:], 4)           // eh_max = 4 (inline limit)
	// eh_depth = 0 (leaf), eh_generation = 0
	// Leaf extent entry at offset 12:
	//   ee_block = 0, ee_len = 1, ee_start_hi = 0, ee_start_lo = rootDirBlock
	le.PutUint16(extBuf[16:], 1)                  // ee_len
	le.PutUint32(extBuf[20:], g0RootDirDataBlock) // ee_start_lo
	if _, err := formatWriteAt(f, 0, nil, rootInodeOff, rootRaw); err != nil {
		f.Close()
		return nil, fmt.Errorf("ext4: format: write root inode: %w", err)
	}

	// ── 5. Root directory data block ─────────────────────────────────────────
	dirBuf := make([]byte, fmtBlockSize)
	// Entry 1: "." → inode 2, rec_len = 12.
	le.PutUint32(dirBuf[0:], RootIno)
	le.PutUint16(dirBuf[4:], 12)
	dirBuf[6] = 1     // name_len
	dirBuf[7] = FtDir // file_type
	dirBuf[8] = '.'
	// Entry 2: ".." → inode 2, rec_len fills the rest of the block.
	ddotRecLen := uint16(fmtBlockSize - 12)
	le.PutUint32(dirBuf[12:], RootIno)
	le.PutUint16(dirBuf[16:], ddotRecLen)
	dirBuf[18] = 2     // name_len
	dirBuf[19] = FtDir // file_type
	dirBuf[20] = '.'
	dirBuf[21] = '.'
	rootDirOff := int64(g0RootDirDataBlock) * fmtBlockSize
	if _, err := formatWriteAt(f, 0, nil, rootDirOff, dirBuf); err != nil {
		f.Close()
		return nil, fmt.Errorf("ext4: format: write root directory: %w", err)
	}

	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("ext4: format: close: %w", err)
	}

	// Open the newly formatted filesystem using the package-level hook so
	// tests can override `formatOpenFS` if needed.
	fs, err := formatOpenFS(path, -1)
	if err != nil {
		return nil, fmt.Errorf("ext4: format: open formatted image: %w", err)
	}
	return fs, nil
}
