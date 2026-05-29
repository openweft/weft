package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
)

// rename moves the filesystem object at oldPath to newPath.
//
// Semantics:
//   - If newPath does not exist the entry is simply moved.
//   - If newPath exists and has the same type as oldPath it is replaced
//     atomically (for directories the target must be empty).
//   - Renaming a directory across parent directories updates the ".." entry
//     inside the moved directory and adjusts link counts.
func rename(f readerWriterAt, fsOffset int64, sb *superblock, oldPath, newPath string) error {
	// --- resolve source ---
	oldParent, oldName, err := lookupParent(f, fsOffset, sb, oldPath)
	if err != nil {
		return fmt.Errorf("ext4: rename: source %q: %w", oldPath, err)
	}

	// --- resolve destination parent (target need not exist yet) ---
	newParent, newName, err := lookupParent(f, fsOffset, sb, newPath)
	if err != nil {
		return fmt.Errorf("ext4: rename: destination parent for %q: %w", newPath, err)
	}

	// No-op when old and new are identical.
	if oldParent.num == newParent.num && oldName == newName {
		return nil
	}

	// Serialise concurrent directory modifications. When the two parents are
	// different, acquire locks in inode-number order to prevent deadlocks.
	if oldParent.num == newParent.num {
		ml := getDirModLock(f, oldParent.num)
		ml.Lock()
		defer ml.Unlock()
	} else {
		first, second := oldParent.num, newParent.num
		if second < first {
			first, second = second, first
		}
		ml1 := getDirModLock(f, first)
		ml2 := getDirModLock(f, second)
		ml1.Lock()
		defer ml1.Unlock()
		ml2.Lock()
		defer ml2.Unlock()
	}

	var srcIno *inode
	oldEntries, err := readDir(f, fsOffset, sb, oldParent)
	if err != nil {
		return fmt.Errorf("ext4: rename: source %q: %w", oldPath, err)
	}
	for _, e := range oldEntries {
		if e.Name != oldName {
			continue
		}
		srcIno, err = readInode(f, fsOffset, sb, e.Inode)
		if err != nil {
			return fmt.Errorf("ext4: rename: source %q: %w", oldPath, err)
		}
		break
	}
	if srcIno == nil {
		return fmt.Errorf("ext4: rename: source %q: not found", oldPath)
	}

	// --- handle existing destination ---
	newEntries, err := readDir(f, fsOffset, sb, newParent)
	if err != nil {
		return err
	}
	for _, e := range newEntries {
		if e.Name != newName {
			continue
		}
		dstIno, err := readInode(f, fsOffset, sb, e.Inode)
		if err != nil {
			return err
		}
		if srcIno.isDir() && !dstIno.isDir() {
			return fmt.Errorf("ext4: rename: cannot replace non-directory %q with a directory", newPath)
		}
		if !srcIno.isDir() && dstIno.isDir() {
			return fmt.Errorf("ext4: rename: cannot replace directory %q with a non-directory", newPath)
		}
		if dstIno.isDir() {
			if err := removeDir(f, fsOffset, sb, newPath); err != nil {
				return err
			}
		} else {
			if err := removeFile(f, fsOffset, sb, newPath); err != nil {
				return err
			}
		}
		break
	}

	// --- determine file type for the new directory entry ---
	fileType := uint8(FtRegFile)
	if srcIno.isDir() {
		fileType = FtDir
	} else if srcIno.isSymlink() {
		fileType = FtSymlink
	}

	// --- add new entry, remove old entry ---
	if err := addDirEntry(f, fsOffset, sb, newParent, srcIno.num, newName, fileType); err != nil {
		return err
	}
	if err := zeroDirEntry(f, fsOffset, sb, oldParent, oldName); err != nil {
		return err
	}

	// --- cross-parent directory move: update ".." and link counts ---
	if srcIno.isDir() && oldParent.num != newParent.num {
		if err := updateDotDot(f, fsOffset, sb, srcIno, newParent.num); err != nil {
			return err
		}
		le := binary.LittleEndian

		// Old parent loses one link (the ".." from the moved dir).
		oldLinks := le.Uint16(oldParent.raw[inodeOffLinksCount:])
		if oldLinks > 0 {
			le.PutUint16(oldParent.raw[inodeOffLinksCount:], oldLinks-1)
		}
		if err := writeInode(f, fsOffset, sb, oldParent); err != nil {
			return err
		}

		// New parent gains one link (the ".." now points to it).
		newLinks := le.Uint16(newParent.raw[inodeOffLinksCount:])
		le.PutUint16(newParent.raw[inodeOffLinksCount:], newLinks+1)
		if err := writeInode(f, fsOffset, sb, newParent); err != nil {
			return err
		}
	}

	return nil
}

// updateDotDot scans the first directory block of dirIno and rewrites the
// inode number of the ".." entry to newParentIno.
func updateDotDot(f readerWriterAt, fsOffset int64, sb *superblock, dirIno *inode, newParentIno uint32) error {
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
			modified := false
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
				if nameLen == 2 && off+10 <= len(buf) && buf[off+8] == '.' && buf[off+9] == '.' {
					le.PutUint32(buf[off:], newParentIno)
					modified = true
					break
				}
				off += recLen
			}
			if modified {
				updateDirBlockCsum(buf, sb, dirIno)
				return writeRawBlock(f, fsOffset, sb, physBlock, buf)
			}
		}
	}
	return nil
}
