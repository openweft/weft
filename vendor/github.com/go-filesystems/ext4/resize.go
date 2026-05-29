package filesystem_ext4

import (
	"fmt"
	"sync/atomic"
)

// Grow increases the logical size of the backing image (grow-only). This is
// a scoped, explicit API entry point for implementing online resize behavior.
//
// Current implementation: validation and plan computation only. Full
// metadata updates (superblock, block group descriptors, bitmaps, checksums)
// are TODO and will be implemented in follow-up work.
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

	// Add any entirely-new block groups.
	for g := oldGroups; g < newGroups; g++ {
		groupStart := uint64(g)*uint64(fs.sb.BlocksPerGroup) + firstData
		// Reserve: block bitmap, inode bitmap, inode table blocks.
		blockBitmapBlock := groupStart
		inodeBitmapBlock := groupStart + 1
		inodeTableBlock := groupStart + 2

		reserved := uint64(2) + inodeTableBlocks
		if reserved > uint64(fs.sb.BlocksPerGroup) {
			return fmt.Errorf("ext4: not enough space in group %d for metadata (reserved=%d groupsz=%d)", g, reserved, fs.sb.BlocksPerGroup)
		}

		// Build block bitmap: mark metadata blocks used, rest free.
		bmap := make([]byte, int(fs.sb.BlockSize))
		for i := uint64(0); i < reserved; i++ {
			setBit(bmap, int(i))
		}

		// Prepare descriptor with initial free counts.
		d := &bgd{raw: make([]byte, int(fs.sb.DescSize)), BlockBitmapBlock: blockBitmapBlock, InodeBitmapBlock: inodeBitmapBlock, InodeTableBlock: inodeTableBlock, FreeBlocksCount: uint32(uint64(fs.sb.BlocksPerGroup) - reserved), FreeInodesCount: fs.sb.InodesPerGroup, UsedDirsCount: 0}

		// Write block and inode bitmaps. Acquire BGD then bitmap locks in
		// canonical order, release BGD before performing IO, and finally
		// update the BGD.
		unlockBGD := lockBGDGroup(rw, g)
		unlockBitmap := lockBitmapGroup(rw, g, true)
		// write block bitmap while holding bitmap lock (BGD already held,
		// but released before IO inside writeBitmapBufNoLock path).
		unlockBGD()
		if err := writeBitmapBufNoLock(rw, fs.partOffset, fs.sb, g, d, true, blockBitmapBlock, bmap); err != nil {
			unlockBitmap()
			return fmt.Errorf("ext4: write new block bitmap for group %d: %w", g, err)
		}
		unlockBitmap()

		// Inode bitmap
		imap := make([]byte, int(fs.sb.BlockSize))
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

		totalAddedBlocks += uint64(fs.sb.BlocksPerGroup) - reserved
	}

	// Update superblock counts and persist.
	// Update counter fields atomically to avoid races with concurrent
	// alloc/free operations.
	atomic.StoreUint64(&fs.sb.BlocksCount, newBlocks)
	atomic.AddUint64(&fs.sb.FreeBlocksCount, totalAddedBlocks)
	if err := writeSuperblock(rw, fs.partOffset, fs.sb); err != nil {
		return fmt.Errorf("ext4: write updated superblock: %w", err)
	}
	return nil
}
