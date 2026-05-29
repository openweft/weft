package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
)

var removeDirChildDir func(readerWriterAt, int64, *superblock, string) error
var removeDirChildFile = removeFile
var removeDirFreeBlocks = freeInodeBlocks
var removeDirFreeSlot = freeInodeSlot

func init() {
	removeDirChildDir = removeDir
}

// DeleteFile removes the regular file at path from the ext4 filesystem.
// The directory entry is zeroed and the inode (with its data blocks) is freed
// when the link count drops to zero. Returns nil if path does not exist
// (idempotent).
func (fs *ext4FS) DeleteFile(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return removeFile(fs.f, fs.partOffset, fs.sb, path)
}

// removeFile orchestrates the full file-removal sequence.
func removeFile(f readerWriterAt, fsOffset int64, sb *superblock, path string) error {
	parent, name, err := lookupParent(f, fsOffset, sb, path)
	if err != nil {
		return nil // parent not found — already gone, idempotent
	}
	target, err := lookupPath(f, fsOffset, sb, path)
	if err != nil {
		return nil // entry not found — already gone, idempotent
	}
	if !target.isRegular() {
		return fmt.Errorf("ext4: DeleteFile: %q is not a regular file", path)
	}
	if err := zeroDirEntry(f, fsOffset, sb, parent, name); err != nil {
		return err
	}
	return decrementAndFreeInode(f, fsOffset, sb, target)
}

// zeroDirEntry finds the directory entry named name inside dirIno and sets its
// inode field to 0. A zeroed inode marks the slot as free without disturbing
// the surrounding layout.
func zeroDirEntry(f readerWriterAt, fsOffset int64, sb *superblock, dirIno *inode, name string) error {
	exts, err := dirIno.extents(f, fsOffset, sb)
	if err != nil {
		return err
	}
	le := binary.LittleEndian
	for _, e := range exts {
		for blk := uint64(0); blk < uint64(e.Count); blk++ {
			physBlock := e.PhysBlock + blk
			buf, err := readRawBlock(f, fsOffset, sb, physBlock)
			if err != nil {
				return err
			}
			if zeroEntryInBlock(buf, name, le) {
				// Trace the directory zeroing (deletion) for debugging.
				traceDirOp("zero", dirIno.num, 0, name)
				updateDirBlockCsum(buf, sb, dirIno)
				return writeRawBlock(f, fsOffset, sb, physBlock, buf)
			}
		}
	}
	return nil // entry not found — idempotent
}

// zeroEntryInBlock scans a directory data block and zeroes the inode of the
// entry matching name. Returns true when an entry was modified.
func zeroEntryInBlock(buf []byte, name string, le binary.ByteOrder) bool {
	off := 0
	for off+8 <= len(buf) {
		recLen := int(le.Uint16(buf[off+4:]))
		if recLen < 8 {
			break
		}
		if buf[off+7] == FtDirTail {
			break
		}
		nameLen := int(buf[off+6])
		if nameLen > 0 && string(buf[off+8:off+8+nameLen]) == name {
			le.PutUint32(buf[off:], 0) // zero inode marks entry as free
			return true
		}
		off += recLen
	}
	return false
}

// decrementAndFreeInode decrements the inode's link count and, when it reaches
// zero, frees the data blocks and the inode slot itself.
func decrementAndFreeInode(f readerWriterAt, fsOffset int64, sb *superblock, in *inode) error {
	// Serialize operations that modify inode metadata and free blocks/slot
	// using a per-inode exclusive lock to avoid exposing transient states
	// to concurrent readers while avoiding serializing all operations on
	// the backing file.
	owner := NewOwner()
	il := getInodeLock(f, in.num)
	il.LockOwner(owner)
	defer il.UnlockOwner(owner)
	le := binary.LittleEndian
	oldLinks := le.Uint16(in.raw[inodeOffLinksCount:])
	newLinks := oldLinks
	if newLinks > 0 {
		newLinks--
	}
	le.PutUint16(in.raw[inodeOffLinksCount:], newLinks)
	// Test-only trace: report the decrement and potential free of the inode.
	traceDecrementAndFreeInode(f, fsOffset, sb, in.num, oldLinks, newLinks)
	if newLinks > 0 {
		return writeInode(f, fsOffset, sb, in)
	}
	if err := freeInodeBlocks(f, fsOffset, sb, in); err != nil {
		return err
	}
	return freeInodeSlot(f, fsOffset, sb, in.num)
}

// freeInodeSlot marks inode inodeNum as free in its block group bitmap.
func freeInodeSlot(f readerWriterAt, fsOffset int64, sb *superblock, inodeNum uint32) error {
	idx := inodeNum - 1
	g := idx / sb.InodesPerGroup
	bit := int(idx % sb.InodesPerGroup)
	// Acquire locks in canonical order: per-group BGD -> bitmap. Prepare
	// descriptor and bitmap under both locks, release BGD before IO, then
	// write bitmap (while holding bitmap lock) and finally persist the BGD
	// and superblock.
	unlockBGD := lockBGDGroup(f, g)
	unlockBitmap := lockBitmapGroup(f, g, false)

	// Re-read under locks to avoid races.
	d, err := readBGD(f, fsOffset, sb, g)
	if err != nil {
		unlockBitmap()
		unlockBGD()
		return err
	}
	bmap, err := readBitmap(f, fsOffset, sb, d.InodeBitmapBlock)
	if err != nil {
		unlockBitmap()
		unlockBGD()
		return err
	}
	clearBit(bmap, bit)
	d.FreeInodesCount++

	// Release BGD before performing IO. Keep bitmap locked and write it
	// without re-acquiring the bitmap lock inside writeBitmapBufNoLock.
	unlockBGD()
	if err := writeBitmapBufNoLock(f, fsOffset, sb, g, d, false, d.InodeBitmapBlock, bmap); err != nil {
		unlockBitmap()
		return err
	}
	// release bitmap lock so writeBGD can take per-group descriptor lock
	// in canonical order when persisting the descriptor.
	unlockBitmap()

	if err := writeBGD(f, fsOffset, sb, g, d); err != nil {
		return err
	}
	atomic.AddUint32(&sb.FreeInodesCount, 1)
	traceFreeInodeSlot(f, fsOffset, sb, inodeNum)
	return writeSuperblock(f, fsOffset, sb)
}

// removeDir removes the directory at path and all its contents recursively.
// Returns nil if the path does not exist (idempotent).
func removeDir(f readerWriterAt, fsOffset int64, sb *superblock, path string) error {
	parentIno, name, err := lookupParent(f, fsOffset, sb, path)
	if err != nil {
		return nil // parent gone — idempotent
	}
	target, err := lookupPath(f, fsOffset, sb, path)
	if err != nil {
		return nil // already gone
	}
	if !target.isDir() {
		return fmt.Errorf("ext4: DeleteDir: %q is not a directory", path)
	}

	// Recursively remove all children.
	children, err := readDir(f, fsOffset, sb, target)
	if err != nil {
		return err
	}
	for _, e := range children {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		childPath := path + "/" + e.Name
		switch e.FileType {
		case FtDir:
			if err := removeDirChildDir(f, fsOffset, sb, childPath); err != nil {
				return err
			}
		default:
			if err := removeDirChildFile(f, fsOffset, sb, childPath); err != nil {
				return err
			}
		}
	}

	// Zero the directory's entry in the parent.
	if err := zeroDirEntry(f, fsOffset, sb, parentIno, name); err != nil {
		return err
	}

	// Free the directory data blocks and inode slot.
	if err := removeDirFreeBlocks(f, fsOffset, sb, target); err != nil {
		return err
	}
	if err := removeDirFreeSlot(f, fsOffset, sb, target.num); err != nil {
		return err
	}

	// Decrement parent link count: the ".." from the deleted directory is gone.
	le := binary.LittleEndian
	parentLinks := le.Uint16(parentIno.raw[inodeOffLinksCount:])
	if parentLinks > 0 {
		le.PutUint16(parentIno.raw[inodeOffLinksCount:], parentLinks-1)
	}
	return writeInode(f, fsOffset, sb, parentIno)
}
