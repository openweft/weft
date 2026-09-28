package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// Verify implementation of the optional write-at-an-offset interface.
//
// The assertion is on the File, not on the filesystem: writability is a
// property of the opened object, and this is what a caller's
// `f.(filesystem.WritableFile)` probe finds.
var _ filesystem.WritableFile = (*ext4File)(nil)

// readOnlyFile is the File this driver returns for a layout it can read but
// cannot write at an offset: an inline-data inode, and the classic ext2/ext3
// indirect block map.
//
// It exists as a separate type rather than as a refusal inside WriteAt because
// filesystem.WritableFile is probed with a type assertion, and the probe is
// the caller's only chance to learn the truth BEFORE it commits to a strategy.
// A File that satisfied the interface and then failed every call would force an
// NFS or WebDAV server to discover the limitation one failed request at a time;
// a File that does not satisfy it makes the server fall back to
// ReadFile + splice + WriteFile, which is slow but correct. Refusing at the
// probe is the difference between "slower" and "broken".
//
// Everything on the read side is delegated unchanged.
type readOnlyFile struct{ inner *ext4File }

var _ filesystem.File = (*readOnlyFile)(nil)

func (r *readOnlyFile) ReadAt(p []byte, off int64) (int, error) { return r.inner.ReadAt(p, off) }
func (r *readOnlyFile) Size() int64                             { return r.inner.Size() }
func (r *readOnlyFile) Close() error                            { return r.inner.Close() }

// WriteAt writes len(p) bytes at off, in place.
//
// This is the method the whole type exists for. Before it, a positional write
// on ext4 could only be expressed as ReadFile + splice + WriteFile — and this
// driver's WriteFile FREES EVERY BLOCK OF THE FILE AND REALLOCATES THEM, so a
// client writing a file in fixed-size blocks paid O(filesize) per block and
// O(n²) overall, plus a full reallocation each time. Here the extent map
// resolved at OpenFile turns a byte offset into a physical block by binary
// search, and the cost is the bytes the caller asked for, plus one allocation
// for the blocks that were not mapped yet.
//
// It follows io.WriterAt to the letter: all of p or a non-nil error, never a
// short write with a nil error. It DOES extend the file, and it fills holes.
//
// # Sparseness is preserved, deliberately
//
// A write far past the end of the file allocates blocks ONLY for the range it
// writes. The gap in between stays a hole — no block, no I/O, no space — and
// reads back as zeros through both ReadAt and ReadFile, which is what ext4
// means by a sparse file and what a caller extending a file by seeking
// expects. FAT has to allocate the gap; ext4 does not, and pretending
// otherwise would waste the space the format was designed to save.
//
// # What it will not do
//
// The extent tree this driver writes is one leaf block deep, so a file whose
// map needs more leaves than fit in a block is refused rather than encoded
// wrongly — the same ceiling WriteFile has always had. Inline-data and
// block-map inodes never reach here: OpenFile hands those back as a plain
// File (see readOnlyFile).
//
// Concurrency: the File's own lock is held exclusively, so concurrent WriteAt
// calls are serialised — stricter than io.WriterAt requires, and correct for
// overlapping ranges too.
func (f *ext4File) WriteAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("ext4: WriteAt: negative offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	// off is caller-supplied; compute the end before anything derives a block
	// number from it, so an overflow is an error rather than a negative
	// offset far inside the image.
	end := off + int64(len(p))
	if end < off {
		return 0, fmt.Errorf("ext4: WriteAt: offset %d + %d bytes overflows int64", off, len(p))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.fs.mu.RLock()
	defer f.fs.mu.RUnlock()
	rw := getRW(f.fs)

	// Map every block the write touches, allocating the ones that are holes,
	// and record the new size. Nothing is written to the caller's bytes until
	// the map is settled, so a failed allocation leaves the file untouched.
	if err := f.mapRangeLocked(rw, off, end); err != nil {
		return 0, err
	}

	n := 0
	for n < len(p) {
		cur := off + int64(n)
		want := int64(len(p) - n)
		phys, avail, ok := f.locateLocked(cur)
		if !ok {
			// mapRangeLocked has just guaranteed coverage of [off, end), so
			// this is unreachable by any correct path; report it rather than
			// index into nothing.
			return n, fmt.Errorf("ext4: WriteAt: offset %d unmapped after allocation in inode %d", cur, f.inodeNum)
		}
		chunk := min(want, avail)
		m, err := rw.WriteAt(p[n:n+int(chunk)], phys)
		n += m
		if err != nil {
			return n, fmt.Errorf("ext4: write at offset %d: %w", cur, err)
		}
	}
	return n, nil
}

// Truncate resizes the file to size bytes.
//
// Growing costs no blocks at all: ext4 files are sparse, so the new region is
// a hole that reads as zeros, and only i_size changes. The one byte range that
// does need writing is the slack between the old end of file and the end of
// the block holding it — those bytes are inside an allocated block and would
// otherwise become readable as whatever they last held.
//
// Shrinking frees every block past the new end and zeroes the slack in the
// last block kept, for the same reason.
func (f *ext4File) Truncate(size int64) error {
	if f.closed.Load() {
		return os.ErrClosed
	}
	if size < 0 {
		return fmt.Errorf("ext4: Truncate: negative size %d", size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if size == f.size {
		return nil
	}
	f.fs.mu.RLock()
	defer f.fs.mu.RUnlock()
	rw := getRW(f.fs)

	if size < f.size {
		return f.shrinkLocked(rw, size)
	}
	if err := f.zeroSlackLocked(rw, f.size, size); err != nil {
		return err
	}
	return f.commitLocked(rw, f.extents, size)
}

// Sync reports whether everything written through this File has reached the
// backing store.
//
// This driver buffers nothing of its own on the positional-write path: WriteAt
// has already issued the data write, and the extent tree and inode with it,
// before it returns. Sync's job is therefore to push the layer beneath, and
// unlike the FAT drivers there is no probe to do first — blockDevice, the type
// every ext4FS is opened over, has Sync in its contract. For the ordinary
// image-backed case that is an *os.File and this is fsync(2); for a caller's
// own BlockDevice it is whatever that implementation promises, and this
// method's guarantee is exactly, and only, that one.
//
// A server answering NFSv3 COMMIT can therefore report FILE_SYNC honestly on a
// file-backed image, which is the case that matters.
//
// Note what it does NOT claim: this is not a journal checkpoint. The journal,
// where one is active, commits its own transactions; Sync makes the bytes
// durable, it does not make a half-finished sequence of writes atomic.
func (f *ext4File) Sync() error {
	if f.closed.Load() {
		return os.ErrClosed
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if err := f.fs.f.Sync(); err != nil {
		return fmt.Errorf("ext4: sync image: %w", err)
	}
	return nil
}

// locateLocked maps a byte offset to a physical byte offset in the image and
// says how many contiguous bytes follow it inside the same extent. f.mu must
// be held.
//
// It is the same binary search ReadAt performs; the difference is that a miss
// here is a caller error rather than a hole to zero-fill, because a write must
// never invent a block.
func (f *ext4File) locateLocked(cur int64) (physOff, avail int64, ok bool) {
	logBlock := uint64(cur / f.blockSize)
	within := cur % f.blockSize
	i := sort.Search(len(f.extents), func(i int) bool {
		e := f.extents[i]
		return uint64(e.LogBlock)+uint64(e.Count) > logBlock
	})
	if i == len(f.extents) || uint64(f.extents[i].LogBlock) > logBlock {
		return 0, 0, false
	}
	e := f.extents[i]
	skip := logBlock - uint64(e.LogBlock)
	phys := e.PhysBlock + skip
	avail = (int64(e.Count)-int64(skip))*f.blockSize - within
	return f.fs.partOffset + int64(phys)*f.blockSize + within, avail, true
}

// mapRangeLocked makes every block covering [off, end) exist, allocating the
// ones that do not, and records the file's new size. f.mu must be held.
//
// The order is deliberate: allocate, zero the freshly allocated blocks, zero
// the slack left behind the old end of file, and only then rewrite the extent
// tree and the inode. A failure before the last step leaves blocks marked in
// use that nothing references — which fsck reports and repairs — rather than a
// file whose i_size claims bytes no block holds, which is data loss.
func (f *ext4File) mapRangeLocked(rw readerWriterAt, off, end int64) error {
	firstBlock := uint64(off / f.blockSize)
	lastBlock := uint64((end - 1) / f.blockSize)

	// Which logical blocks in the range are not mapped yet.
	var missing []uint64
	for b := firstBlock; b <= lastBlock; b++ {
		if _, _, ok := f.locateLocked(int64(b) * f.blockSize); !ok {
			missing = append(missing, b)
		}
	}
	newSize := max(f.size, end)
	if len(missing) == 0 {
		if newSize == f.size {
			return nil
		}
		if err := f.zeroSlackLocked(rw, f.size, newSize); err != nil {
			return err
		}
		return f.commitLocked(rw, f.extents, newSize)
	}

	phys, err := allocBlocks(rw, f.fs.partOffset, f.fs.sb, uint32(len(missing)))
	if err != nil {
		return fmt.Errorf("ext4: alloc blocks for inode %d: %w", f.inodeNum, err)
	}
	// A freshly allocated block holds whatever the last file to own it left
	// there. Every byte of it that the caller is not about to overwrite must
	// read as zero, and the cheapest way to guarantee that for a partially
	// written block is to zero the whole block first.
	zero := make([]byte, f.blockSize)
	for _, b := range phys {
		if err := writeRawBlock(rw, f.fs.partOffset, f.fs.sb, b, zero); err != nil {
			return fmt.Errorf("ext4: zero new block %d: %w", b, err)
		}
	}

	merged := make([]extentLeaf, len(f.extents), len(f.extents)+len(missing))
	copy(merged, f.extents)
	for i, b := range missing {
		merged = append(merged, extentLeaf{LogBlock: uint32(b), PhysBlock: phys[i], Count: 1})
	}
	merged = coalesceExtents(merged)

	if err := f.zeroSlackLocked(rw, f.size, newSize); err != nil {
		return err
	}
	return f.commitLocked(rw, merged, newSize)
}

// zeroSlackLocked zeroes the bytes between oldSize and the end of the block
// that holds them, when growing to newSize would otherwise expose them.
// f.mu must be held.
//
// Those bytes live inside a block the file already owns but outside its
// length, so nothing has ever had to define them. Growing the file gives them
// a meaning, and the only correct meaning is zero — a grow that revealed the
// previous tenant's data would be an information leak across files.
func (f *ext4File) zeroSlackLocked(rw readerWriterAt, oldSize, newSize int64) error {
	if newSize <= oldSize || oldSize == 0 {
		return nil
	}
	slack := oldSize % f.blockSize
	if slack == 0 {
		return nil
	}
	phys, avail, ok := f.locateLocked(oldSize)
	if !ok {
		// The old end of file fell in a hole: there is no stale data to
		// hide, because there is no block.
		return nil
	}
	n := min(f.blockSize-slack, min(avail, newSize-oldSize))
	if _, err := rw.WriteAt(make([]byte, n), phys); err != nil {
		return fmt.Errorf("ext4: zero slack at offset %d: %w", oldSize, err)
	}
	return nil
}

// shrinkLocked cuts the file to size bytes, freeing every block past the new
// end. f.mu must be held.
//
// The metadata is rewritten BEFORE the blocks are freed: a failure partway
// then leaves blocks marked in use that the file no longer claims, which fsck
// repairs, rather than a file still pointing at blocks marked free, which the
// next allocation would hand to someone else.
func (f *ext4File) shrinkLocked(rw readerWriterAt, size int64) error {
	keepBlocks := uint64((size + f.blockSize - 1) / f.blockSize)
	var kept []extentLeaf
	var freed []uint64
	for _, e := range f.extents {
		switch {
		case uint64(e.LogBlock) >= keepBlocks:
			for i := uint64(0); i < uint64(e.Count); i++ {
				freed = append(freed, e.PhysBlock+i)
			}
		case uint64(e.LogBlock)+uint64(e.Count) <= keepBlocks:
			kept = append(kept, e)
		default:
			n := keepBlocks - uint64(e.LogBlock)
			kept = append(kept, extentLeaf{LogBlock: e.LogBlock, PhysBlock: e.PhysBlock, Count: uint16(n)})
			for i := n; i < uint64(e.Count); i++ {
				freed = append(freed, e.PhysBlock+i)
			}
		}
	}
	// Zero the slack in the last block kept, so a later grow reads zeros
	// rather than the bytes that used to be past the new end.
	if slack := size % f.blockSize; slack != 0 {
		if phys, avail, ok := f.locateLocked(size); ok {
			n := min(f.blockSize-slack, avail)
			if _, err := rw.WriteAt(make([]byte, n), phys); err != nil {
				return fmt.Errorf("ext4: zero slack at offset %d: %w", size, err)
			}
		}
	}
	if err := f.commitLocked(rw, kept, size); err != nil {
		return err
	}
	for _, b := range freed {
		if err := freeBlock(rw, f.fs.partOffset, f.fs.sb, b); err != nil {
			return fmt.Errorf("ext4: free block %d: %w", b, err)
		}
	}
	return nil
}

// commitLocked persists a new extent map and size for the file, then adopts
// them as this File's own state. f.mu must be held.
//
// The inode is re-read under the per-inode lock rather than cached in the
// File, exactly as OpenFile does: another writer may have changed the mode,
// the owner or the timestamps in between, and a File that wrote back a stale
// copy of the whole inode would silently undo their work.
func (f *ext4File) commitLocked(rw readerWriterAt, exts []extentLeaf, size int64) error {
	owner := NewOwner()
	il := getInodeLock(rw, f.inodeNum)
	il.LockOwner(owner)
	defer il.UnlockOwner(owner)

	in, err := openFileReadInode(rw, f.fs.partOffset, f.fs.sb, f.inodeNum)
	if err != nil {
		return err
	}
	// The extent tree this driver writes is at most one leaf block deep, so
	// the previous leaf block — if the inode had one — becomes garbage the
	// moment the new root is written. Free it, but only when the tree has the
	// exact shape this driver produces (depth 1, one index entry): a deeper
	// tree, from mke2fs, has interior blocks this code does not enumerate,
	// and freeing a block it has not identified would be corruption. Leaking
	// one is merely wasteful.
	oldChild, hadChild := singleExtentChild(in)

	if err := writeExtentTree(rw, f.fs.partOffset, f.fs.sb, in, exts); err != nil {
		return err
	}
	in.setSize(uint64(size))
	var blocks uint32
	for _, e := range exts {
		blocks += uint32(e.Count)
	}
	in.setBlocks512(blocks * (f.fs.sb.BlockSize / 512))
	le := binary.LittleEndian
	now := uint32(time.Now().Unix())
	le.PutUint32(in.raw[8:], now)  // i_atime
	le.PutUint32(in.raw[12:], now) // i_ctime
	le.PutUint32(in.raw[16:], now) // i_mtime
	if err := writeInode(rw, f.fs.partOffset, f.fs.sb, in); err != nil {
		return err
	}
	if hadChild {
		if newChild, ok := singleExtentChild(in); !ok || newChild != oldChild {
			if err := freeBlock(rw, f.fs.partOffset, f.fs.sb, oldChild); err != nil {
				return fmt.Errorf("ext4: free stale extent leaf block %d: %w", oldChild, err)
			}
		}
	}

	f.extents = exts
	f.size = size
	return nil
}

// singleExtentChild returns the block holding the inode's extent leaves, when
// the tree has exactly the shape writeExtentTree produces: depth 1 with a
// single index entry in the inode. Any other shape returns false, because this
// code cannot enumerate the interior blocks of a tree it did not build.
func singleExtentChild(in *inode) (uint64, bool) {
	buf := in.raw[inodeOffBlock : inodeOffBlock+60]
	le := binary.LittleEndian
	if le.Uint16(buf[0:]) != ExtentMagic {
		return 0, false
	}
	if le.Uint16(buf[6:]) != 1 || le.Uint16(buf[2:]) != 1 {
		return 0, false
	}
	return uint64(le.Uint32(buf[16:])) | uint64(le.Uint16(buf[20:]))<<32, true
}

// coalesceExtents sorts leaves by logical block and merges runs that are
// adjacent both logically and physically.
//
// Merging is not cosmetic: writeExtentTree encodes the leaves into ONE block,
// so the number of leaves is a hard limit on how large a file this driver can
// describe. A file appended to in small writes produces one leaf per write
// unless the runs that the allocator placed side by side are folded back
// together, and hitting the limit would turn a working file into a refused
// write. A run is also capped at 0x7FFF blocks, the on-disk field's maximum.
func coalesceExtents(exts []extentLeaf) []extentLeaf {
	if len(exts) < 2 {
		return exts
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i].LogBlock < exts[j].LogBlock })
	out := exts[:1]
	for _, e := range exts[1:] {
		last := &out[len(out)-1]
		if uint64(last.LogBlock)+uint64(last.Count) == uint64(e.LogBlock) &&
			last.PhysBlock+uint64(last.Count) == e.PhysBlock &&
			uint64(last.Count)+uint64(e.Count) <= 0x7FFF {
			last.Count += e.Count
			continue
		}
		out = append(out, e)
	}
	return out
}
