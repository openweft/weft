package filesystem_ext4

import (
	"fmt"
	"sync/atomic"
)

// CheckImage performs lightweight consistency checks on the filesystem image.
// It verifies that per-group free counts in BGDs match the on-disk bitmaps
// and that the superblock free counts equal the sums of the BGD counts.
func (fs *ext4FS) CheckImage() error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	rw := getRW(fs)
	sb := fs.sb
	if sb == nil {
		return fmt.Errorf("ext4: no superblock loaded")
	}
	nGroups := sb.numBlockGroups()
	var totalFreeBlocks uint64
	var totalFreeInodes uint32
	var errs []string
	for g := uint32(0); g < nGroups; g++ {
		d, err := readBGD(rw, fs.partOffset, sb, g)
		if err != nil {
			return fmt.Errorf("ext4: read BGD %d: %w", g, err)
		}
		bmap, err := readBitmap(rw, fs.partOffset, sb, d.BlockBitmapBlock)
		if err != nil {
			return fmt.Errorf("ext4: read block bitmap g=%d: %w", g, err)
		}
		freeBlocks := countZeroBits(bmap, int(sb.BlocksPerGroup))
		if uint32(freeBlocks) != d.FreeBlocksCount {
			errs = append(errs, fmt.Sprintf("bg %d: free blocks mismatch: bitmap=%d bgd=%d", g, freeBlocks, d.FreeBlocksCount))
		}
		imap, err := readBitmap(rw, fs.partOffset, sb, d.InodeBitmapBlock)
		if err != nil {
			return fmt.Errorf("ext4: read inode bitmap g=%d: %w", g, err)
		}
		freeInodes := countZeroBits(imap, int(sb.InodesPerGroup))
		if uint32(freeInodes) != d.FreeInodesCount {
			errs = append(errs, fmt.Sprintf("bg %d: free inodes mismatch: bitmap=%d bgd=%d", g, freeInodes, d.FreeInodesCount))
		}
		totalFreeBlocks += uint64(freeBlocks)
		totalFreeInodes += uint32(freeInodes)
	}
	if totalFreeBlocks != atomic.LoadUint64(&sb.FreeBlocksCount) {
		errs = append(errs, fmt.Sprintf("superblock free blocks mismatch: sum=%d sb=%d", totalFreeBlocks, atomic.LoadUint64(&sb.FreeBlocksCount)))
	}
	if totalFreeInodes != atomic.LoadUint32(&sb.FreeInodesCount) {
		errs = append(errs, fmt.Sprintf("superblock free inodes mismatch: sum=%d sb=%d", totalFreeInodes, atomic.LoadUint32(&sb.FreeInodesCount)))
	}
	if len(errs) > 0 {
		return fmt.Errorf("ext4: consistency errors:\n%s", joinLines(errs))
	}
	return nil
}

// RepairImage attempts to repair simple inconsistencies by recomputing the
// per-group and superblock free counts from on-disk bitmaps. It does not
// attempt deep repairs such as reconstructing allocation maps from inodes.
func (fs *ext4FS) RepairImage() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	rw := getRW(fs)
	sb := fs.sb
	if sb == nil {
		return fmt.Errorf("ext4: no superblock loaded")
	}
	nGroups := sb.numBlockGroups()
	var totalFreeBlocks uint64
	var totalFreeInodes uint32
	for g := uint32(0); g < nGroups; g++ {
		d, err := readBGD(rw, fs.partOffset, sb, g)
		if err != nil {
			return fmt.Errorf("ext4: read BGD %d: %w", g, err)
		}
		bmap, err := readBitmap(rw, fs.partOffset, sb, d.BlockBitmapBlock)
		if err != nil {
			return fmt.Errorf("ext4: read block bitmap g=%d: %w", g, err)
		}
		freeBlocks := countZeroBits(bmap, int(sb.BlocksPerGroup))
		if uint32(freeBlocks) != d.FreeBlocksCount {
			d.FreeBlocksCount = uint32(freeBlocks)
			if err := writeBGD(rw, fs.partOffset, sb, g, d); err != nil {
				return fmt.Errorf("ext4: write updated BGD %d: %w", g, err)
			}
		}
		imap, err := readBitmap(rw, fs.partOffset, sb, d.InodeBitmapBlock)
		if err != nil {
			return fmt.Errorf("ext4: read inode bitmap g=%d: %w", g, err)
		}
		freeInodes := countZeroBits(imap, int(sb.InodesPerGroup))
		if uint32(freeInodes) != d.FreeInodesCount {
			d.FreeInodesCount = uint32(freeInodes)
			if err := writeBGD(rw, fs.partOffset, sb, g, d); err != nil {
				return fmt.Errorf("ext4: write updated BGD %d: %w", g, err)
			}
		}
		totalFreeBlocks += uint64(freeBlocks)
		totalFreeInodes += uint32(freeInodes)
	}
	if totalFreeBlocks != atomic.LoadUint64(&sb.FreeBlocksCount) || totalFreeInodes != atomic.LoadUint32(&sb.FreeInodesCount) {
		atomic.StoreUint64(&sb.FreeBlocksCount, totalFreeBlocks)
		atomic.StoreUint32(&sb.FreeInodesCount, totalFreeInodes)
		if err := writeSuperblock(rw, fs.partOffset, sb); err != nil {
			return fmt.Errorf("ext4: write updated superblock: %w", err)
		}
	}
	return nil
}

func countZeroBits(b []byte, maxBits int) int {
	count := 0
	for i := 0; i < maxBits; i++ {
		if b[i/8]&(1<<uint(i%8)) == 0 {
			count++
		}
	}
	return count
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
