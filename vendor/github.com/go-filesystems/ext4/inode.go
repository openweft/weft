package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"time"
)

const (
	inodeOffMode       = 0
	inodeOffSizeLo     = 4
	inodeOffLinksCount = 26
	inodeOffFlags      = 32
	inodeOffBlock      = 40 // 60 bytes: extent tree or block map
	inodeOffGeneration = 100
	inodeOffFilACLLo   = 104
	inodeOffSizeHi     = 108
	inodeOffBlocksHi   = 116 // part of osd2
	inodeOffCsumLo     = 124 // l_i_checksum_lo (offset within osd2)
	inodeOffExtraIsize = 128
	inodeOffCsumHi     = 130
)

// inode wraps the raw bytes of an on-disk ext4 inode.
type inode struct {
	raw  []byte
	num  uint32
	size uint64
	mode uint16
}

// readInode reads inode inodeNum from the filesystem.
func readInode(f readerWriterAt, fsOffset int64, sb *superblock, inodeNum uint32) (*inode, error) {
	if inodeNum == 0 {
		return nil, fmt.Errorf("ext4: inode 0 is invalid")
	}
	// Reject inode numbers beyond the total inodes count.
	if sb != nil && inodeNum > sb.InodesCount {
		return nil, fmt.Errorf("ext4: inode %d out of range", inodeNum)
	}
	// Inodes are numbered starting at 1.
	idx := inodeNum - 1
	g := idx / sb.InodesPerGroup
	localIdx := idx % sb.InodesPerGroup

	// To avoid holding BGD locks while performing IO and to reduce
	// contention, use a read-then-lock-verify/retry strategy: read the
	// block-group descriptor to compute the inode offset, acquire the
	// inode-table lock, re-check the descriptor is unchanged, then read
	// the inode. Retry a few times if the descriptor moves under us.
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		d1, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			return nil, err
		}
		off := int64(d1.InodeTableBlock)*int64(sb.BlockSize) + int64(localIdx)*int64(sb.InodeSize)

		// Acquire inode-table lock (canonical order intent: BGD -> inode),
		// but avoid holding BGD here to reduce contention. After locking,
		// verify that the descriptor did not change.
		unlockInode := lockInodeTableGroup(f, g)
		d2, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			unlockInode()
			return nil, err
		}
		if d1.InodeTableBlock != d2.InodeTableBlock {
			// Descriptor moved; release and retry with backoff.
			unlockInode()
			time.Sleep(time.Duration(50+attempt*20) * time.Microsecond)
			continue
		}

		raw := make([]byte, sb.InodeSize)
		if _, err := f.ReadAt(raw, fsOffset+off); err != nil {
			unlockInode()
			return nil, fmt.Errorf("ext4: read inode %d: %w", inodeNum, err)
		}
		unlockInode()

		le := binary.LittleEndian
		in := &inode{raw: raw, num: inodeNum}
		in.mode = le.Uint16(raw[inodeOffMode:])
		sizeLo := uint64(le.Uint32(raw[inodeOffSizeLo:]))
		sizeHi := uint64(le.Uint32(raw[inodeOffSizeHi:]))
		in.size = (sizeHi << 32) | sizeLo
		return in, nil
	}
	return nil, fmt.Errorf("ext4: unstable BGD mapping reading inode %d", inodeNum)
}

// writeInode writes the inode data back, updating its checksum.
func writeInode(f readerWriterAt, fsOffset int64, sb *superblock, in *inode) error {
	// To reduce per-inode critical section length we prepare the on-disk
	// inode bytes while holding the lock only briefly, then release the
	// lock before performing the potentially expensive journal/IO work.
	// This keeps metadata updates atomic w.r.t. in-memory state while
	// avoiding long-held mutexes during disk operations.
	// No pre-read required here; addInodeToTx will compute offsets as needed.

	// (previously we read the on-disk inode size for diagnostics; not needed)

	// Delegate to helper that can optionally add the inode bytes into an
	// existing transaction. For the common case we pass nil which causes
	// the helper to perform the usual journaled-or-direct write.
	if err := addInodeToTx(nil, f, fsOffset, sb, in); err != nil {
		return fmt.Errorf("ext4: write inode %d: %w", in.num, err)
	}
	return nil
}

// inodeDiskOffset returns the absolute file offset for the on-disk inode
// entry for inodeNum.
func inodeDiskOffset(f readerWriterAt, fsOffset int64, sb *superblock, inodeNum uint32) (int64, error) {
	if inodeNum == 0 {
		return 0, fmt.Errorf("ext4: inode 0 is invalid")
	}
	idx := inodeNum - 1
	g := idx / sb.InodesPerGroup
	localIdx := idx % sb.InodesPerGroup
	d, err := readBGD(f, fsOffset, sb, g)
	if err != nil {
		return 0, err
	}
	off := int64(d.InodeTableBlock)*int64(sb.BlockSize) + int64(localIdx)*int64(sb.InodeSize)
	return off, nil
}

// addInodeToTx prepares the on-disk inode bytes under the inode lock and
// either appends them into the supplied transaction `tx` or writes them
// directly when tx is nil. This lets callers group inode writes together
// with other metadata blocks in a single atomic transaction.
func addInodeToTx(tx *Transaction, f readerWriterAt, fsOffset int64, sb *superblock, in *inode) error {
	idx := in.num - 1
	g := idx / sb.InodesPerGroup
	localIdx := idx % sb.InodesPerGroup

	d, err := readBGD(f, fsOffset, sb, g)
	if err != nil {
		return err
	}
	off := int64(d.InodeTableBlock)*int64(sb.BlockSize) + int64(localIdx)*int64(sb.InodeSize)

	// Short critical section: compute checksum (if any) and copy the raw
	// inode bytes to a local buffer while holding the inode lock.
	il := getInodeLock(f, in.num)
	owner := il.Owner()
	if owner == 0 {
		owner = NewOwner()
	}
	il.LockOwner(owner)
	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		computeInodeCsum(sb, in)
	}
	traceInodeWrite(f, fsOffset, sb, in.num, 0, in.size)
	writeBuf := make([]byte, len(in.raw))
	copy(writeBuf, in.raw)
	il.UnlockOwner(owner)

	// If a transaction was supplied, add the inode range into it; otherwise
	// perform the usual journaled/direct write path.
	if tx != nil {
		return addRangeToTx(tx, f, fsOffset, sb, fsOffset+off, writeBuf)
	}
	return applyOrJournalWrite(f, fsOffset, sb, fsOffset+off, writeBuf)
}

// flags returns the inode flags.
func (in *inode) flags() uint32 {
	return binary.LittleEndian.Uint32(in.raw[inodeOffFlags:])
}

// isDir reports whether this inode is a directory.
func (in *inode) isDir() bool { return in.mode&0xF000 == 0x4000 }

// isRegular reports whether this inode is a regular file.
func (in *inode) isRegular() bool { return in.mode&0xF000 == 0x8000 }

// isSymlink reports whether this inode is a symlink.
func (in *inode) isSymlink() bool { return in.mode&0xF000 == 0xA000 }

// physicalBlocks returns the ordered list of (physBlock, logBlock, count)
// triples that describe the file's data layout.
// physBlock = physical (image) block number.
type extentLeaf struct {
	LogBlock  uint32
	PhysBlock uint64
	Count     uint16 // number of blocks in this run
}

// extents traverses the extent tree and returns all leaf extents.
func (in *inode) extents(f readerWriterAt, fsOffset int64, sb *superblock) ([]extentLeaf, error) {
	flags := in.flags()
	if flags&InodeFlagInlineData != 0 {
		return nil, fmt.Errorf("ext4: inline data not supported")
	}
	if flags&InodeFlagExtents == 0 {
		// Mutating callers only support extent-mapped inodes. Read paths use
		// readExtents, which also handles classic ext2/ext3 block maps.
		return nil, fmt.Errorf("ext4: old-style block map not supported (inode %d)", in.num)
	}
	return parseExtentNode(f, fsOffset, sb, in.raw[inodeOffBlock:inodeOffBlock+60], in.num, in.raw)
}

// readExtents returns the data-block layout of an inode for read-only paths.
// Unlike extents() — used by mutating callers, which only support extent
// trees — it also synthesises a layout for classic ext2/ext3 indirect block
// maps (EXT4_EXTENTS_FL clear). Inline data is still reported explicitly.
func (in *inode) readExtents(f readerWriterAt, fsOffset int64, sb *superblock) ([]extentLeaf, error) {
	flags := in.flags()
	if flags&InodeFlagInlineData != 0 {
		return nil, fmt.Errorf("ext4: inline data not supported")
	}
	if flags&InodeFlagExtents == 0 {
		return in.blockMapExtents(f, fsOffset, sb)
	}
	return parseExtentNode(f, fsOffset, sb, in.raw[inodeOffBlock:inodeOffBlock+60], in.num, in.raw)
}

// parseExtentNode parses a 60-byte (or block-sized) extent node buffer.
func parseExtentNode(f readerWriterAt, fsOffset int64, sb *superblock, buf []byte, inodeNum uint32, inodeRaw []byte) ([]extentLeaf, error) {
	le := binary.LittleEndian
	magic := le.Uint16(buf[0:])
	if magic != ExtentMagic {
		// Test-only diagnostics: when encountering a bad extent magic,
		// record the raw i_block bytes and a stack trace to help identify
		// concurrent writers that may have corrupted the inode table.
		debugPrintf("DEBUG parseExtentNode: bad extent magic 0x%04X in inode %d\n", magic, inodeNum)
		// Print a short sample of the i_block buffer for context.
		max := len(buf)
		if max > 60 {
			max = 60
		}
		debugPrintf("DEBUG parseExtentNode: inode=%d i_block sample (first %d bytes): %q\n", inodeNum, max, string(buf[:max]))
		var st [4096]byte
		n := runtime.Stack(st[:], false)
		debugPrintf("DEBUG parseExtentNode stack:\n%s\n", string(st[:n]))
		return nil, fmt.Errorf("ext4: bad extent magic 0x%04X in inode %d", magic, inodeNum)
	}
	entries := le.Uint16(buf[2:])
	depth := le.Uint16(buf[6:])

	var leaves []extentLeaf
	for i := uint16(0); i < entries; i++ {
		entOff := 12 + int(i)*12
		if entOff+12 > len(buf) {
			break
		}
		e := buf[entOff:]
		if depth == 0 {
			// Leaf entry.
			logBlock := le.Uint32(e[0:])
			count := le.Uint16(e[4:])
			// count > 32768 means uninitialized extent; use count & 0x7FFF.
			if count > 32768 {
				count &= 0x7FFF
			}
			physHi := uint64(le.Uint16(e[6:]))
			physLo := uint64(le.Uint32(e[8:]))
			leaves = append(leaves, extentLeaf{
				LogBlock:  logBlock,
				PhysBlock: (physHi << 32) | physLo,
				Count:     count,
			})
		} else {
			// Index entry: navigate to child block.
			leafLo := uint64(le.Uint32(e[4:]))
			leafHi := uint64(le.Uint16(e[8:]))
			childBlock := (leafHi << 32) | leafLo
			childBuf, err := readRawBlock(f, fsOffset, sb, childBlock)
			if err != nil {
				return nil, fmt.Errorf("ext4: read extent index block %d: %w", childBlock, err)
			}
			sub, err := parseExtentNode(f, fsOffset, sb, childBuf, inodeNum, inodeRaw)
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, sub...)
		}
	}
	return leaves, nil
}

// readFileData reads the full content of a regular file.
func readFileData(f readerWriterAt, fsOffset int64, sb *superblock, in *inode) ([]byte, error) {
	// Use a per-inode exclusive lock while we consult the extent tree and
	// read data so writers cannot race. Avoid the coarse file-level lock to
	// prevent serializing reads/writes across the entire backing file.
	il := getInodeLock(f, in.num)
	owner := il.Owner()
	if owner == 0 {
		owner = NewOwner()
	}
	il.LockOwner(owner)
	defer il.UnlockOwner(owner)

	// Re-read the on-disk inode bytes while holding the inode lock to avoid
	// observing partially-applied inode updates (flags vs i_block). This
	// ensures the in-memory `in` reflects the durable state before we parse
	// extents and read data. Some test scaffolding constructs a minimal
	// superblock with zeroed group counts; guard against zero-valued fields
	// to avoid divide-by-zero panics and treat that case as "no on-disk
	// inode re-read".
	if sb != nil && sb.InodesPerGroup != 0 && sb.BlockSize != 0 && sb.InodeSize != 0 {
		idx := in.num - 1
		g := idx / sb.InodesPerGroup
		localIdx := idx % sb.InodesPerGroup

		const maxAttempts = 8
		for attempt := 0; attempt < maxAttempts; attempt++ {
			d1, err := readBGD(f, fsOffset, sb, g)
			if err != nil {
				break
			}
			off := int64(d1.InodeTableBlock)*int64(sb.BlockSize) + int64(localIdx)*int64(sb.InodeSize)

			unlockInode := lockInodeTableGroup(f, g)
			d2, err := readBGD(f, fsOffset, sb, g)
			if err != nil {
				unlockInode()
				break
			}
			if d1.InodeTableBlock != d2.InodeTableBlock {
				unlockInode()
				time.Sleep(time.Duration(50+attempt*20) * time.Microsecond)
				continue
			}
			raw := make([]byte, sb.InodeSize)
			if _, rerr := f.ReadAt(raw, fsOffset+off); rerr == nil {
				in.raw = raw
				le := binary.LittleEndian
				in.mode = le.Uint16(in.raw[inodeOffMode:])
				sizeLo := uint64(le.Uint32(in.raw[inodeOffSizeLo:]))
				sizeHi := uint64(le.Uint32(in.raw[inodeOffSizeHi:]))
				in.size = (sizeHi << 32) | sizeLo
			}
			unlockInode()
			break
		}
	}

	// Inline data: content lives in the inode (i_block + system.data xattr)
	// rather than in data blocks.
	if in.isInline() {
		return in.inlineData(f, fsOffset, sb)
	}
	// Classic ext2/ext3 indirect block map (EXT4_EXTENTS_FL clear).
	if in.flags()&InodeFlagExtents == 0 && in.flags()&InodeFlagInlineData == 0 {
		return in.blockMapData(f, fsOffset, sb)
	}

	ext, err := in.extents(f, fsOffset, sb)
	if err != nil {
		return nil, err
	}
	out := make([]byte, in.size)
	written := 0
	// If the inode reports a non-zero size but there are no extents, treat
	// this as an error rather than returning an empty slice silently.
	if int(in.size) > 0 && len(ext) == 0 {
		return nil, fmt.Errorf("ext4: inode %d has size %d but no extents", in.num, in.size)
	}
	// No debug logging in the read path.
	for _, e := range ext {
		for blk := uint64(0); blk < uint64(e.Count); blk++ {
			physBlock := e.PhysBlock + blk
			blockOff := int64(physBlock) * int64(sb.BlockSize)
			remain := int(in.size) - written
			if remain <= 0 {
				break
			}
			n := int(sb.BlockSize)
			if n > remain {
				n = remain
			}
			if _, err := f.ReadAt(out[written:written+n], fsOffset+blockOff); err != nil {
				return nil, fmt.Errorf("ext4: read data block %d: %w", physBlock, err)
			}
			written += n
		}
	}
	if written < int(in.size) {
		return nil, fmt.Errorf("ext4: short read: got %d bytes, expected %d", written, in.size)
	}
	return out[:written], nil
}

// setSize updates the inode size fields.
func (in *inode) setSize(size uint64) {
	le := binary.LittleEndian
	old := in.size
	le.PutUint32(in.raw[inodeOffSizeLo:], uint32(size&0xFFFFFFFF))
	le.PutUint32(in.raw[inodeOffSizeHi:], uint32(size>>32))
	in.size = size
	// Test-only hook: record size changes so we can track truncations.
	traceSetSize(in.num, old, size)
}

// setBlocks512 updates the i_blocks_lo field (in units of 512-byte sectors).
func (in *inode) setBlocks512(n uint32) {
	binary.LittleEndian.PutUint32(in.raw[28:], n)
}

// setMode sets mode + link count.
func (in *inode) setMode(mode uint16, links uint16) {
	binary.LittleEndian.PutUint16(in.raw[inodeOffMode:], mode)
	binary.LittleEndian.PutUint16(in.raw[inodeOffLinksCount:], links)
	in.mode = mode
}

// setGeneration sets i_generation.
func (in *inode) generation() uint32 {
	return binary.LittleEndian.Uint32(in.raw[inodeOffGeneration:])
}

// setInlineExtents encodes a depth-0 extent tree directly in i_block.
// At most 4 extents fit inline.
func (in *inode) setInlineExtents(exts []extentLeaf) error {
	if len(exts) > 4 {
		return fmt.Errorf("ext4: too many extents for inline tree (%d > 4)", len(exts))
	}
	le := binary.LittleEndian
	buf := in.raw[inodeOffBlock : inodeOffBlock+60]
	// Clear the block.
	for i := range buf {
		buf[i] = 0
	}
	// Header.
	le.PutUint16(buf[0:], ExtentMagic)
	le.PutUint16(buf[2:], uint16(len(exts))) // eh_entries
	le.PutUint16(buf[4:], 4)                 // eh_max (inline = 4)
	le.PutUint16(buf[6:], 0)                 // eh_depth = 0
	le.PutUint32(buf[8:], 0)                 // eh_generation
	// Leaf entries.
	for i, e := range exts {
		off := 12 + i*12
		le.PutUint32(buf[off:], e.LogBlock)
		le.PutUint16(buf[off+4:], e.Count)
		le.PutUint16(buf[off+6:], uint16(e.PhysBlock>>32))
		le.PutUint32(buf[off+8:], uint32(e.PhysBlock&0xFFFFFFFF))
	}
	// Set EXT4_EXTENTS_FL.
	flags := binary.LittleEndian.Uint32(in.raw[inodeOffFlags:])
	flags |= InodeFlagExtents
	binary.LittleEndian.PutUint32(in.raw[inodeOffFlags:], flags)
	return nil
}

// computeInodeCsum computes and stores the inode checksum.
func computeInodeCsum(sb *superblock, in *inode) {
	le := binary.LittleEndian
	raw := in.raw

	// Zero checksum fields.
	origLo := le.Uint16(raw[inodeOffCsumLo:])
	le.PutUint16(raw[inodeOffCsumLo:], 0)
	var origHi uint16
	if len(raw) > inodeOffCsumHi+2 {
		origHi = le.Uint16(raw[inodeOffCsumHi:])
		le.PutUint16(raw[inodeOffCsumHi:], 0)
	}

	numBuf := make([]byte, 4)
	le.PutUint32(numBuf, in.num)
	genBuf := make([]byte, 4)
	le.PutUint32(genBuf, in.generation())

	seed := sb.csumSeed()
	csum := crc32c(seed, numBuf)
	csum = crc32c(csum, genBuf)
	csum = crc32c(csum, raw)

	le.PutUint16(raw[inodeOffCsumLo:], uint16(csum&0xFFFF))
	if len(raw) > inodeOffCsumHi+2 {
		le.PutUint16(raw[inodeOffCsumHi:], uint16(csum>>16))
	}
	_ = origLo
	_ = origHi
}
