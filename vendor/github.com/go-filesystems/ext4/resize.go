package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
)

// Grow increases the logical size of the backing image (grow-only). It is a
// scoped, explicit API entry point for online resize.
//
// The backing file is extended and the full metadata update path runs:
// new block groups are appended (backup superblock + GDT + reserved-GDT in
// sparse-super groups, block/inode bitmaps, inode tables), the trailing
// partial group's bitmap is patched, and the primary superblock, block
// group descriptors, and checksums are refreshed to reflect the grown
// layout. See Shrink for the reverse operation and Resize for the
// grow-or-shrink dispatcher.
func (fs *ext4FS) Grow(newSizeBytes int64) error {
	if newSizeBytes <= 0 {
		return fmt.Errorf("ext4: invalid new size %d", newSizeBytes)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Sanity/check current sizes.
	curSize, err := fs.f.Size()
	if err != nil {
		return fmt.Errorf("ext4: stat backing file: %w", err)
	}
	if newSizeBytes <= curSize {
		return fmt.Errorf("ext4: new size must be greater than current size (cur=%d new=%d)", curSize, newSizeBytes)
	}

	if fs.sb == nil || fs.sb.BlockSize == 0 {
		return fmt.Errorf("ext4: invalid superblock/blocksize")
	}

	blockSize := int64(fs.sb.BlockSize)
	oldBlocks := fs.sb.BlocksCount
	curBlocks := uint64(curSize) / uint64(blockSize)
	newBlocks := uint64(newSizeBytes) / uint64(blockSize)
	if newBlocks <= curBlocks {
		return fmt.Errorf("ext4: new size does not add full blocks (curBlocks=%d newBlocks=%d)", curBlocks, newBlocks)
	}

	// Compute group counts.
	oldGroups := fs.sb.numBlockGroups()
	newGroups := uint32((newBlocks + uint64(fs.sb.BlocksPerGroup) - 1) / uint64(fs.sb.BlocksPerGroup))

	// Extend backing file first (makes space available to the host).
	if err := fs.f.Truncate(newSizeBytes); err != nil {
		return fmt.Errorf("ext4: truncate backing file: %w", err)
	}

	// Publish the new block count up front so numBlockGroups() reflects the
	// grown layout while we write descriptors and bitmaps for the new groups
	// (writeBGD and the commit pipeline validate against numBlockGroups).
	atomic.StoreUint64(&fs.sb.BlocksCount, newBlocks)

	rw := getRW(fs)
	firstData := uint64(fs.sb.FirstDataBlock)
	blockSizeU := uint64(fs.sb.BlockSize)
	inodeTableBlocks := (uint64(fs.sb.InodeSize)*uint64(fs.sb.InodesPerGroup) + blockSizeU - 1) / blockSizeU

	var totalAddedBlocks uint64

	// If the last existing group is partially filled, update its bitmap
	// to account for additional blocks within that same group.
	if oldGroups > 0 {
		lastOld := oldGroups - 1
		groupStart := uint64(lastOld)*uint64(fs.sb.BlocksPerGroup) + firstData
		var oldGroupUsed uint64
		if oldBlocks > groupStart {
			oldGroupUsed = oldBlocks - groupStart
		}
		var newGroupUsed uint64
		if newBlocks > groupStart {
			// cap at group size
			cap := uint64(fs.sb.BlocksPerGroup)
			newGroupUsed = newBlocks - groupStart
			if newGroupUsed > cap {
				newGroupUsed = cap
			}
		}
		if oldGroupUsed < newGroupUsed {
			// Acquire locks in canonical order: BGD then bitmap. Hold bitmap
			// while performing the bitmap write, but release the BGD lock
			// before performing IO to avoid holding descriptor locks across
			// long-running writes.
			unlockBGD := lockBGDGroup(rw, lastOld)
			unlockBitmap := lockBitmapGroup(rw, lastOld, true)

			// Re-read descriptor and bitmap under the locks to avoid races.
			d, err := readBGD(rw, fs.partOffset, fs.sb, lastOld)
			if err != nil {
				unlockBitmap()
				unlockBGD()
				return fmt.Errorf("ext4: read BGD for lastOld group: %w", err)
			}
			bmap, err := readBitmap(rw, fs.partOffset, fs.sb, d.BlockBitmapBlock)
			if err != nil {
				unlockBitmap()
				unlockBGD()
				return fmt.Errorf("ext4: read block bitmap: %w", err)
			}
			for i := oldGroupUsed; i < newGroupUsed; i++ {
				clearBit(bmap, int(i))
			}
			added := uint32(newGroupUsed - oldGroupUsed)
			d.FreeBlocksCount += added

			// Release BGD before IO, keep bitmap locked and write without
			// acquiring the bitmap lock again.
			unlockBGD()
			if err := writeBitmapBufNoLock(rw, fs.partOffset, fs.sb, lastOld, d, true, d.BlockBitmapBlock, bmap); err != nil {
				unlockBitmap()
				return fmt.Errorf("ext4: write updated block bitmap: %w", err)
			}
			unlockBitmap()

			if err := writeBGD(rw, fs.partOffset, fs.sb, lastOld, d); err != nil {
				return fmt.Errorf("ext4: write updated BGD: %w", err)
			}
			totalAddedBlocks += uint64(added)
		}
	}

	// Number of GDT blocks needed for the GROWN group count, plus the
	// reserved-GDT-blocks (resize inode) recorded in the superblock. A
	// sparse-super group reserves a backup superblock + this many GDT and
	// reserved-GDT blocks at its start.
	gdtBlocksNew := (uint64(newGroups)*uint64(fs.sb.DescSize) + blockSizeU - 1) / blockSizeU
	reservedGdt := uint64(binary.LittleEndian.Uint16(fs.sb.raw[0xCE:])) // s_reserved_gdt_blocks
	bitsPerBlock := uint64(fs.sb.BlockSize) * 8

	var addedInodes uint32
	var addedFreeInodes uint32

	// Add any entirely-new block groups.
	for g := oldGroups; g < newGroups; g++ {
		groupStart := uint64(g)*uint64(fs.sb.BlocksPerGroup) + firstData

		// The final group may be partial: it spans only the blocks that
		// actually exist, which can be fewer than blocks_per_group.
		groupBlocks := uint64(fs.sb.BlocksPerGroup)
		if groupStart+groupBlocks > newBlocks {
			groupBlocks = newBlocks - groupStart
		}

		// Leading blocks consumed by a backup superblock + GDT + reserved
		// GDT blocks (only in sparse-super groups).
		var superBlocks uint64
		if fs.sb.isSparseSuperGroup(g) {
			superBlocks = 1 + gdtBlocksNew + reservedGdt
		}

		blockBitmapBlock := groupStart + superBlocks
		inodeBitmapBlock := blockBitmapBlock + 1
		inodeTableBlock := blockBitmapBlock + 2

		reserved := superBlocks + 2 + inodeTableBlocks
		if reserved > groupBlocks {
			return fmt.Errorf("ext4: not enough space in group %d for metadata (reserved=%d groupsz=%d)", g, reserved, groupBlocks)
		}

		// Write a backup superblock + GDT into sparse-super groups. The GDT
		// is filled in later once all descriptors are written; here we just
		// stamp the backup superblock so its block is initialised. The
		// definitive backups are refreshed after the descriptor table is
		// complete (see refreshBackups below).
		if superBlocks > 0 {
			sbCopy := make([]byte, len(fs.sb.raw))
			copy(sbCopy, fs.sb.raw)
			binary.LittleEndian.PutUint16(sbCopy[90:], uint16(g)) // s_block_group_nr
			if err := writeRawBlock(rw, fs.partOffset, fs.sb, groupStart, sbCopy); err != nil {
				return fmt.Errorf("ext4: write backup superblock g=%d: %w", g, err)
			}
		}

		// Build block bitmap: leading metadata used, plus all bits beyond the
		// group's real size up to the end of the bitmap block set to 1 (the
		// kernel-style trailing padding e2fsck expects). For a partial final
		// group this also covers the tail bits inside blocks_per_group.
		bmap := make([]byte, int(fs.sb.BlockSize))
		for i := uint64(0); i < reserved; i++ {
			setBit(bmap, int(i))
		}
		for i := groupBlocks; i < bitsPerBlock; i++ {
			setBit(bmap, int(i))
		}

		// Inode bitmap: padding bits beyond inodes-per-group set to 1.
		imap := make([]byte, int(fs.sb.BlockSize))
		for i := uint64(fs.sb.InodesPerGroup); i < bitsPerBlock; i++ {
			setBit(imap, int(i))
		}

		// Prepare descriptor with initial free counts.
		d := &bgd{raw: make([]byte, int(fs.sb.DescSize)), BlockBitmapBlock: blockBitmapBlock, InodeBitmapBlock: inodeBitmapBlock, InodeTableBlock: inodeTableBlock, FreeBlocksCount: uint32(groupBlocks - reserved), FreeInodesCount: fs.sb.InodesPerGroup, UsedDirsCount: 0}
		// bgd.encode() only refreshes the free/used counters, so the bitmap
		// and inode-table block pointers (which are fixed at group creation)
		// must be written into the raw descriptor explicitly.
		le := binary.LittleEndian
		le.PutUint32(d.raw[0:], uint32(blockBitmapBlock))
		le.PutUint32(d.raw[4:], uint32(inodeBitmapBlock))
		le.PutUint32(d.raw[8:], uint32(inodeTableBlock))
		if fs.sb.FeatureIncompat&FeatIncompat64bit != 0 && fs.sb.DescSize >= 64 {
			le.PutUint32(d.raw[32:], uint32(blockBitmapBlock>>32))
			le.PutUint32(d.raw[36:], uint32(inodeBitmapBlock>>32))
			le.PutUint32(d.raw[40:], uint32(inodeTableBlock>>32))
		}

		unlockBGD := lockBGDGroup(rw, g)
		unlockBitmap := lockBitmapGroup(rw, g, true)
		unlockBGD()
		if err := writeBitmapBufNoLock(rw, fs.partOffset, fs.sb, g, d, true, blockBitmapBlock, bmap); err != nil {
			unlockBitmap()
			return fmt.Errorf("ext4: write new block bitmap for group %d: %w", g, err)
		}
		unlockBitmap()

		unlockBGD = lockBGDGroup(rw, g)
		unlockBitmap = lockBitmapGroup(rw, g, false)
		unlockBGD()
		if err := writeBitmapBufNoLock(rw, fs.partOffset, fs.sb, g, d, false, inodeBitmapBlock, imap); err != nil {
			unlockBitmap()
			return fmt.Errorf("ext4: write new inode bitmap for group %d: %w", g, err)
		}
		unlockBitmap()

		// Zero-initialize inode table blocks.
		zero := make([]byte, int(fs.sb.BlockSize))
		for i := uint64(0); i < inodeTableBlocks; i++ {
			if err := writeRawBlock(rw, fs.partOffset, fs.sb, inodeTableBlock+i, zero); err != nil {
				return fmt.Errorf("ext4: write inode table block g=%d off=%d: %w", g, i, err)
			}
		}

		// Write the block group descriptor.
		if err := writeBGD(rw, fs.partOffset, fs.sb, g, d); err != nil {
			return fmt.Errorf("ext4: write BGD for new group %d: %w", g, err)
		}

		totalAddedBlocks += groupBlocks - reserved
		addedInodes += fs.sb.InodesPerGroup
		addedFreeInodes += fs.sb.InodesPerGroup
	}

	// Extend the resize inode (inode 7) so it owns the reserved-GDT blocks of
	// the newly added sparse-super groups. Without this e2fsck reports
	// "Resize inode not valid".
	if reservedGdt > 0 {
		if err := fs.extendResizeInode(rw, oldGroups, newGroups, gdtBlocksNew, reservedGdt); err != nil {
			return fmt.Errorf("ext4: extend resize inode: %w", err)
		}
	}

	// Update superblock counts and persist.
	// Update counter fields atomically to avoid races with concurrent
	// alloc/free operations.
	atomic.StoreUint64(&fs.sb.BlocksCount, newBlocks)
	atomic.AddUint64(&fs.sb.FreeBlocksCount, totalAddedBlocks)
	if addedInodes > 0 {
		fs.sb.InodesCount += addedInodes
		binary.LittleEndian.PutUint32(fs.sb.raw[0:], fs.sb.InodesCount)
		atomic.AddUint32(&fs.sb.FreeInodesCount, addedFreeInodes)
	}
	if err := writeSuperblock(rw, fs.partOffset, fs.sb); err != nil {
		return fmt.Errorf("ext4: write updated superblock: %w", err)
	}

	// Refresh backup superblock + GDT copies in all sparse-super groups so
	// they reflect the new descriptor table and superblock. The primary GDT
	// at the start of the filesystem is kept current by writeBGD above.
	if err := fs.refreshSuperBackups(rw, newGroups, gdtBlocksNew); err != nil {
		return fmt.Errorf("ext4: refresh backups: %w", err)
	}
	return nil
}

// resizeIno is the well-known inode that owns the reserved GDT blocks used
// for online growth (the "resize inode").
const resizeIno uint32 = 7

// extendResizeInode registers the reserved-GDT blocks of newly added
// sparse-super groups with the resize inode.
//
// The resize inode maps reserved GDT blocks through a double-indirect block:
// i_block[13] points to a DIND block whose non-zero entries are the IND
// blocks (these IND blocks are group 0's own reserved-GDT blocks). Entry i of
// the DIND corresponds to reserved-GDT slot i; the IND block it points at then
// lists, one per backup group (in ascending group order, starting at index 0
// for group 1), the matching reserved-GDT block in that group.
//
// For each newly added sparse-super group we append its reserved-GDT block
// numbers at the next per-group slot in every IND block, then grow the resize
// inode's i_blocks count and rewrite it (which refreshes its checksum).
func (fs *ext4FS) extendResizeInode(rw readerWriterAt, oldGroups, newGroups uint32, gdtBlocks, reservedGdt uint64) error {
	// Count how many sparse-super backup groups (>0) precede group g; that is
	// the per-group slot index within each IND block.
	backupOrdinal := func(g uint32) int {
		n := 0
		for x := uint32(1); x < g; x++ {
			if fs.sb.isSparseSuperGroup(x) {
				n++
			}
		}
		return n
	}

	in, err := readInode(rw, fs.partOffset, fs.sb, resizeIno)
	if err != nil {
		return fmt.Errorf("read resize inode: %w", err)
	}
	le := binary.LittleEndian
	bs := int64(fs.sb.BlockSize)

	dindBlock := uint64(le.Uint32(in.raw[inodeOffBlock+13*4:]))
	if dindBlock == 0 {
		return fmt.Errorf("resize inode has no double-indirect block")
	}
	dind := make([]byte, bs)
	if _, err := rw.ReadAt(dind, fs.partOffset+int64(dindBlock)*bs); err != nil {
		return fmt.Errorf("read DIND block: %w", err)
	}

	var addedBlocks uint64
	for g := oldGroups; g < newGroups; g++ {
		if !fs.sb.isSparseSuperGroup(g) {
			continue
		}
		slot := backupOrdinal(g)
		groupStart := uint64(g)*uint64(fs.sb.BlocksPerGroup) + uint64(fs.sb.FirstDataBlock)
		// Group g's reserved-GDT blocks immediately follow its backup
		// superblock (1 block) and its GDT copy (gdtBlocks).
		grpReservedStart := groupStart + 1 + gdtBlocks

		// Iterate every DIND slot. Slot 0 is unused by the resize-inode
		// layout (logical block 0 is the GDT itself, not a reserved-GDT
		// block), so only the non-zero IND pointers correspond to real
		// reserved-GDT blocks that the inode accounts for.
		for i := uint64(0); i < uint64(len(dind))/4; i++ {
			indBlockNum := uint64(le.Uint32(dind[i*4:]))
			if indBlockNum == 0 {
				continue
			}
			indBuf := make([]byte, bs)
			if _, err := rw.ReadAt(indBuf, fs.partOffset+int64(indBlockNum)*bs); err != nil {
				return fmt.Errorf("read IND block %d: %w", indBlockNum, err)
			}
			// The reserved-GDT block in group g for this slot mirrors the
			// offset of the IND block within group 0's GDT region.
			le.PutUint32(indBuf[slot*4:], uint32(grpReservedStart+i-1))
			if err := writeRawBlock(rw, fs.partOffset, fs.sb, indBlockNum, indBuf); err != nil {
				return fmt.Errorf("write IND block %d: %w", indBlockNum, err)
			}
			addedBlocks++
		}
	}

	if addedBlocks == 0 {
		return nil
	}

	// Grow i_blocks (recorded in 512-byte sectors) to account for the newly
	// owned reserved-GDT blocks.
	sectorsPerBlock := uint64(fs.sb.BlockSize) / 512
	iblocksLo := uint64(le.Uint32(in.raw[28:]))
	iblocksHi := uint64(le.Uint16(in.raw[inodeOffBlocksHi:]))
	total := (iblocksHi << 32) | iblocksLo
	total += addedBlocks * sectorsPerBlock
	le.PutUint32(in.raw[28:], uint32(total&0xFFFFFFFF))
	le.PutUint16(in.raw[inodeOffBlocksHi:], uint16(total>>32))

	if err := writeInode(rw, fs.partOffset, fs.sb, in); err != nil {
		return fmt.Errorf("write resize inode: %w", err)
	}
	return nil
}

// refreshSuperBackups rewrites the backup superblock and group-descriptor
// table into every sparse-super group (other than group 0). It reads the
// authoritative primary GDT and superblock and copies them out, stamping each
// backup superblock with its own s_block_group_nr.
func (fs *ext4FS) refreshSuperBackups(rw readerWriterAt, nGroups uint32, gdtBlocks uint64) error {
	blockSizeU := uint64(fs.sb.BlockSize)
	primaryGdtBlock := fs.sb.bgdTableBlock()

	// Read the authoritative GDT bytes.
	gdt := make([]byte, gdtBlocks*blockSizeU)
	if _, err := rw.ReadAt(gdt, fs.partOffset+int64(primaryGdtBlock)*int64(blockSizeU)); err != nil {
		return fmt.Errorf("read primary GDT: %w", err)
	}

	for g := uint32(1); g < nGroups; g++ {
		if !fs.sb.isSparseSuperGroup(g) {
			continue
		}
		groupStart := uint64(g)*uint64(fs.sb.BlocksPerGroup) + uint64(fs.sb.FirstDataBlock)

		sbCopy := make([]byte, len(fs.sb.raw))
		copy(sbCopy, fs.sb.raw)
		binary.LittleEndian.PutUint16(sbCopy[90:], uint16(g)) // s_block_group_nr
		// Backup superblocks live at byte 0 of their group's first block.
		if err := writeRawBlock(rw, fs.partOffset, fs.sb, groupStart, sbCopy); err != nil {
			return fmt.Errorf("write backup superblock g=%d: %w", g, err)
		}
		if _, err := rw.WriteAt(gdt, fs.partOffset+int64(groupStart+1)*int64(blockSizeU)); err != nil {
			return fmt.Errorf("write backup GDT g=%d: %w", g, err)
		}
	}
	return nil
}

// Resize adjusts the logical size of the backing image. It dispatches to
// Grow or Shrink based on the relation between newSizeBytes and the current
// backing size. A request to keep the existing size is a no-op.
func (fs *ext4FS) Resize(newSizeBytes int64) error {
	if newSizeBytes <= 0 {
		return fmt.Errorf("ext4: invalid new size %d", newSizeBytes)
	}
	curSize, err := fs.f.Size()
	if err != nil {
		return fmt.Errorf("ext4: stat backing file: %w", err)
	}
	if newSizeBytes == curSize {
		return nil
	}
	if newSizeBytes > curSize {
		return fs.Grow(newSizeBytes)
	}
	return fs.Shrink(newSizeBytes)
}

// shrinkMinimumBlocks computes the smallest BlocksCount the filesystem could
// be safely shrunk to without losing data. The minimum is the highest used
// data block index + 1 (so all in-use blocks remain addressable), clamped to
// at least one full block group worth of blocks so the resulting image still
// contains a valid superblock + group-0 metadata + a usable data area.
//
// Used blocks are gathered from the per-group block bitmaps. Bits beyond the
// last block belonging to the (possibly partial) trailing group are ignored —
// formatters/Grow mark them as "used" purely as a sentinel and they don't
// represent real data.
func shrinkMinimumBlocks(f readerWriterAt, fsOffset int64, sb *superblock) (uint64, error) {
	if sb == nil || sb.BlockSize == 0 || sb.BlocksPerGroup == 0 {
		return 0, fmt.Errorf("ext4: invalid superblock for shrink")
	}
	nGroups := sb.numBlockGroups()
	totalBlocks := sb.BlocksCount
	firstData := uint64(sb.FirstDataBlock)
	blockSizeU := uint64(sb.BlockSize)
	inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + blockSizeU - 1) / blockSizeU
	// Per-group metadata reservation at the head of every group. This is the
	// (optional, sparse-super) backup superblock + GDT + reserved-GDT blocks,
	// followed by the block bitmap, inode bitmap and inode table. Bits in this
	// range don't count as live data for shrink purposes — they vanish along
	// with the group itself when it's dropped.
	gdtBlocks := (uint64(nGroups)*uint64(sb.DescSize) + blockSizeU - 1) / blockSizeU
	reservedGdt := uint64(binary.LittleEndian.Uint16(sb.raw[0xCE:]))
	highestUsed := uint64(0)
	for g := uint32(0); g < nGroups; g++ {
		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			return 0, fmt.Errorf("ext4: read BGD %d: %w", g, err)
		}
		bmap, err := readBitmap(f, fsOffset, sb, d.BlockBitmapBlock)
		if err != nil {
			return 0, fmt.Errorf("ext4: read block bitmap g=%d: %w", g, err)
		}
		groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + firstData
		// Effective last block (exclusive) for this group: either a full
		// group's worth or the tail of the filesystem.
		groupEnd := groupStart + uint64(sb.BlocksPerGroup)
		if groupEnd > totalBlocks {
			groupEnd = totalBlocks
		}
		groupRange := groupEnd - groupStart
		// Scan the bitmap from the high end down to the per-group head
		// reservation; the first set bit gives the highest live block in
		// this group. Group 0 also reserves the superblock+BGDT blocks,
		// but those sit *below* the per-group head-reservation anyway, so
		// they don't affect the high-end scan.
		max := int(groupRange)
		if max > len(bmap)*8 {
			max = len(bmap) * 8
		}
		// Head reservation for this specific group: sparse-super groups also
		// reserve a backup superblock + GDT + reserved-GDT blocks before the
		// bitmaps/inode table.
		var superBlocks uint64
		if sb.isSparseSuperGroup(g) {
			superBlocks = 1 + gdtBlocks + reservedGdt
		}
		headReserved := superBlocks + 2 + inodeTableBlocks
		floor := int(headReserved)
		if floor > max {
			floor = max
		}
		for i := max - 1; i >= floor; i-- {
			if bmap[i/8]&(1<<uint(i%8)) != 0 {
				cand := groupStart + uint64(i) + 1
				if cand > highestUsed {
					highestUsed = cand
				}
				break
			}
		}
	}
	// Always keep at least one full block group so the filesystem retains a
	// valid superblock + BGD table + group-0 layout.
	minByLayout := uint64(sb.BlocksPerGroup)
	if highestUsed < minByLayout {
		highestUsed = minByLayout
	}
	return highestUsed, nil
}

// Shrink reduces the logical size of the backing image. The filesystem is
// resized down to newSizeBytes; the operation refuses if newSizeBytes is
// below the minimum needed to keep all currently-used blocks addressable.
//
// Scope: this implementation performs a non-relocating shrink. It frees the
// trailing block groups (and the tail of the trailing partial group) only
// when those blocks are already free. This matches the common resize2fs
// behaviour for an idle filesystem where in-use data sits below the shrink
// boundary. Callers should free or move large files prior to a shrink that
// would otherwise be rejected.
//
// The metadata updates are journaled-friendly: per-group BGDs and the
// trailing-group bitmap are written through the standard helpers (which go
// through the commit dispatcher / journal), and the superblock is persisted
// last. The backing file is truncated only after all metadata writes have
// been acknowledged so e2fsck sees a consistent image even if a crash
// interrupts the truncate.
func (fs *ext4FS) Shrink(newSizeBytes int64) error {
	if newSizeBytes <= 0 {
		return fmt.Errorf("ext4: invalid new size %d", newSizeBytes)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	curSize, err := fs.f.Size()
	if err != nil {
		return fmt.Errorf("ext4: stat backing file: %w", err)
	}
	if newSizeBytes >= curSize {
		return fmt.Errorf("ext4: new size must be smaller than current size (cur=%d new=%d)", curSize, newSizeBytes)
	}

	if fs.sb == nil || fs.sb.BlockSize == 0 || fs.sb.BlocksPerGroup == 0 {
		return fmt.Errorf("ext4: invalid superblock/blocksize")
	}

	blockSize := int64(fs.sb.BlockSize)
	if newSizeBytes%blockSize != 0 {
		return fmt.Errorf("ext4: new size %d is not a multiple of block size %d", newSizeBytes, blockSize)
	}
	newBlocks := uint64(newSizeBytes) / uint64(blockSize)
	oldBlocks := fs.sb.BlocksCount
	if newBlocks >= oldBlocks {
		return fmt.Errorf("ext4: new size does not drop any blocks (oldBlocks=%d newBlocks=%d)", oldBlocks, newBlocks)
	}

	rw := getRW(fs)

	// Enforce the per-resize2fs minimum: refuse if the new size would
	// drop any currently-used block.
	minBlocks, err := shrinkMinimumBlocks(rw, fs.partOffset, fs.sb)
	if err != nil {
		return err
	}
	if newBlocks < minBlocks {
		return fmt.Errorf("ext4: shrink refused: requested %d blocks but minimum is %d (used data extends beyond truncation point)", newBlocks, minBlocks)
	}

	oldGroups := fs.sb.numBlockGroups()
	newGroups := uint32((newBlocks + uint64(fs.sb.BlocksPerGroup) - 1) / uint64(fs.sb.BlocksPerGroup))
	if newGroups == 0 {
		return fmt.Errorf("ext4: shrink would leave zero block groups")
	}
	firstData := uint64(fs.sb.FirstDataBlock)
	blockSizeU := uint64(fs.sb.BlockSize)
	inodeTableBlocks := (uint64(fs.sb.InodeSize)*uint64(fs.sb.InodesPerGroup) + blockSizeU - 1) / blockSizeU

	// Tally how many blocks/inodes we will lose so the superblock counters
	// can be updated atomically once all per-group writes are complete.
	var freedBlocks uint64
	var droppedInodes uint32
	var droppedFreeInodes uint32

	// 1) Update the trailing (still-present) group's bitmap. Blocks beyond
	//    the new boundary inside this group are marked "used" so allocators
	//    can't hand them out; the corresponding FreeBlocksCount is reduced.
	if newGroups > 0 {
		lastNew := newGroups - 1
		groupStart := uint64(lastNew)*uint64(fs.sb.BlocksPerGroup) + firstData
		// Old usable count inside the trailing group (may itself have been
		// partial if oldBlocks didn't align to BlocksPerGroup).
		var oldGroupUsed uint64
		if oldBlocks > groupStart {
			oldGroupUsed = oldBlocks - groupStart
			if oldGroupUsed > uint64(fs.sb.BlocksPerGroup) {
				oldGroupUsed = uint64(fs.sb.BlocksPerGroup)
			}
		}
		var newGroupUsed uint64
		if newBlocks > groupStart {
			newGroupUsed = newBlocks - groupStart
			if newGroupUsed > uint64(fs.sb.BlocksPerGroup) {
				newGroupUsed = uint64(fs.sb.BlocksPerGroup)
			}
		}
		if newGroupUsed < oldGroupUsed {
			unlockBGD := lockBGDGroup(rw, lastNew)
			unlockBitmap := lockBitmapGroup(rw, lastNew, true)
			d, err := readBGD(rw, fs.partOffset, fs.sb, lastNew)
			if err != nil {
				unlockBitmap()
				unlockBGD()
				return fmt.Errorf("ext4: read BGD for trailing group: %w", err)
			}
			bmap, err := readBitmap(rw, fs.partOffset, fs.sb, d.BlockBitmapBlock)
			if err != nil {
				unlockBitmap()
				unlockBGD()
				return fmt.Errorf("ext4: read trailing block bitmap: %w", err)
			}
			// Tally how many bits in [newGroupUsed, oldGroupUsed) were
			// previously free; those vanish from FreeBlocksCount when
			// the trailing group shrinks. The bits themselves are LEFT
			// CLEAR — e2fsck flags any set bit beyond the new
			// BlocksCount boundary as inconsistent (this is what
			// resize2fs does too).
			var prevFree uint32
			for i := newGroupUsed; i < oldGroupUsed; i++ {
				if bmap[i/8]&(1<<uint(i%8)) == 0 {
					prevFree++
				}
				clearBit(bmap, int(i))
			}
			if d.FreeBlocksCount < prevFree {
				return fmt.Errorf("ext4: trailing group BGD free count (%d) < expected free in tail (%d)", d.FreeBlocksCount, prevFree)
			}
			d.FreeBlocksCount -= prevFree
			freedBlocks += uint64(prevFree)

			unlockBGD()
			if err := writeBitmapBufNoLock(rw, fs.partOffset, fs.sb, lastNew, d, true, d.BlockBitmapBlock, bmap); err != nil {
				unlockBitmap()
				return fmt.Errorf("ext4: write trailing block bitmap: %w", err)
			}
			unlockBitmap()
			if err := writeBGD(rw, fs.partOffset, fs.sb, lastNew, d); err != nil {
				return fmt.Errorf("ext4: write trailing BGD: %w", err)
			}
		}
	}

	// 2) For every group we are dropping entirely, verify it has no
	//    in-use data blocks and no allocated inodes, then deduct its
	//    bookkeeping from the totals.
	for g := newGroups; g < oldGroups; g++ {
		d, err := readBGD(rw, fs.partOffset, fs.sb, g)
		if err != nil {
			return fmt.Errorf("ext4: read BGD %d during shrink: %w", g, err)
		}
		bmap, err := readBitmap(rw, fs.partOffset, fs.sb, d.BlockBitmapBlock)
		if err != nil {
			return fmt.Errorf("ext4: read block bitmap %d: %w", g, err)
		}
		// Expected metadata-reserved blocks at the head of this group,
		// including any sparse-super backup superblock + GDT + reserved-GDT
		// blocks. These vanish with the group when it is dropped.
		var superBlocks uint64
		if fs.sb.isSparseSuperGroup(g) {
			gdtBlocks := (uint64(fs.sb.numBlockGroups())*uint64(fs.sb.DescSize) + blockSizeU - 1) / blockSizeU
			reservedGdt := uint64(binary.LittleEndian.Uint16(fs.sb.raw[0xCE:]))
			superBlocks = 1 + gdtBlocks + reservedGdt
		}
		reserved := superBlocks + 2 + inodeTableBlocks
		groupStart := uint64(g)*uint64(fs.sb.BlocksPerGroup) + firstData
		groupEnd := groupStart + uint64(fs.sb.BlocksPerGroup)
		if groupEnd > oldBlocks {
			groupEnd = oldBlocks
		}
		groupRange := groupEnd - groupStart
		// Any bit beyond `reserved` must be unset for the group to be
		// safely droppable. (The shrinkMinimumBlocks check above already
		// guarantees this, but re-verify under the lock to catch races.)
		maxBits := int(groupRange)
		if maxBits > len(bmap)*8 {
			maxBits = len(bmap) * 8
		}
		for i := int(reserved); i < maxBits; i++ {
			if bmap[i/8]&(1<<uint(i%8)) != 0 {
				return fmt.Errorf("ext4: shrink refused: group %d has used data block at offset %d", g, i)
			}
		}
		// Sum free data blocks within the actually-present range
		// (excluding metadata reserved bits and bits beyond groupRange,
		// which were sentinel-set by format/Grow).
		var groupFree uint32
		for i := int(reserved); i < maxBits; i++ {
			if bmap[i/8]&(1<<uint(i%8)) == 0 {
				groupFree++
			}
		}
		freedBlocks += uint64(groupFree)

		// Inode bookkeeping: confirm the group has no in-use inodes,
		// then deduct.
		ibmap, err := readBitmap(rw, fs.partOffset, fs.sb, d.InodeBitmapBlock)
		if err != nil {
			return fmt.Errorf("ext4: read inode bitmap %d: %w", g, err)
		}
		used, _ := countUsedBits(ibmap, int(fs.sb.InodesPerGroup))
		if used != 0 {
			return fmt.Errorf("ext4: shrink refused: group %d has %d allocated inodes", g, used)
		}
		droppedInodes += fs.sb.InodesPerGroup
		droppedFreeInodes += d.FreeInodesCount
	}

	// 3) Update in-memory superblock counters and persist.
	atomic.StoreUint64(&fs.sb.BlocksCount, newBlocks)
	atomic.AddUint64(&fs.sb.FreeBlocksCount, ^(freedBlocks - 1)) // sub freedBlocks
	if freedBlocks == 0 {
		// addUint64(^uint64(0)-1+1)==-(0) is a no-op; nothing to do.
	}
	// Update inode totals. These fields aren't subject to the same atomic
	// contention as BlocksCount (callers serialise via fs.mu), so a plain
	// assignment is sufficient.
	if droppedInodes > 0 {
		fs.sb.InodesCount -= droppedInodes
		binary.LittleEndian.PutUint32(fs.sb.raw[0:], fs.sb.InodesCount)
		atomic.AddUint32(&fs.sb.FreeInodesCount, ^uint32(droppedFreeInodes-1))
	}
	if err := writeSuperblock(rw, fs.partOffset, fs.sb); err != nil {
		return fmt.Errorf("ext4: write updated superblock: %w", err)
	}

	// 4) Truncate the backing file to the new size last, after metadata
	//    updates have been acknowledged. If a crash interrupts the
	//    truncate the on-disk superblock already advertises the new size
	//    and e2fsck will reconcile by reporting the file is shorter than
	//    s_blocks_count (which it is willing to fix).
	if err := fs.f.Truncate(newSizeBytes); err != nil {
		return fmt.Errorf("ext4: truncate backing file: %w", err)
	}
	return nil
}

// countUsedBits returns the number of set bits in the first maxBits of
// bitmap, used by shrink to verify a group has no allocated inodes before
// dropping it.
func countUsedBits(bitmap []byte, maxBits int) (int, int) {
	used := 0
	max := maxBits
	if max > len(bitmap)*8 {
		max = len(bitmap) * 8
	}
	for i := 0; i < max; i++ {
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			used++
		}
	}
	return used, max
}
