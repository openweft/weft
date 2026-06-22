package filesystem_ext4

// link.go — POSIX hard-link creation. A hard link is a second directory entry
// pointing at an existing inode, with the inode's link count bumped. Directories
// may not be hard-linked (POSIX), and the final component of oldPath is not
// dereferenced (linking a symlink links the symlink inode itself).

import (
	"encoding/binary"
	"fmt"

	filesystem "github.com/go-filesystems/interface"
)

// ext4FS implements the optional hard-link capability.
var _ filesystem.HardLinker = (*ext4FS)(nil)

// Link adds a new directory entry at newPath pointing at the same inode as
// oldPath and bumps that inode's link count. oldPath must not be a directory;
// newPath must not already exist.
func (fs *ext4FS) Link(oldPath, newPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	rw := getRW(fs)
	return makeLink(rw, fs.partOffset, fs.sb, oldPath, newPath)
}

func makeLink(f readerWriterAt, fsOffset int64, sb *superblock, oldPath, newPath string) error {
	// Resolve the source entry's inode (without dereferencing a final symlink).
	srcParent, srcName, err := lookupParent(f, fsOffset, sb, oldPath)
	if err != nil {
		return err
	}
	srcEntries, err := readDir(f, fsOffset, sb, srcParent)
	if err != nil {
		return err
	}
	var srcInoNum uint32
	for _, e := range srcEntries {
		if e.Name == srcName {
			srcInoNum = e.Inode
			break
		}
	}
	if srcInoNum == 0 {
		return fmt.Errorf("ext4: %q not found", oldPath)
	}
	srcIno, err := readInode(f, fsOffset, sb, srcInoNum)
	if err != nil {
		return err
	}
	if srcIno.isDir() {
		return fmt.Errorf("ext4: hard link to directory %q not permitted", oldPath)
	}

	// Mirror the source's directory file-type for the new entry.
	ft := uint8(FtRegFile)
	if srcIno.isSymlink() {
		ft = FtSymlink
	}

	newParent, newName, err := lookupParent(f, fsOffset, sb, newPath)
	if err != nil {
		return err
	}
	dirML := getDirModLock(f, newParent.num)
	dirML.Lock()
	defer dirML.Unlock()
	newParent, err = readInode(f, fsOffset, sb, newParent.num)
	if err != nil {
		return fmt.Errorf("ext4: refresh parent inode %d: %w", newParent.num, err)
	}
	newEntries, err := readDir(f, fsOffset, sb, newParent)
	if err != nil {
		return err
	}
	for _, e := range newEntries {
		if e.Name == newName {
			return fmt.Errorf("ext4: %q already exists", newPath)
		}
	}

	if err := addDirEntry(f, fsOffset, sb, newParent, srcInoNum, newName, ft); err != nil {
		return err
	}

	// Bump the source inode's link count under its per-inode lock. Re-read so
	// the increment is against current on-disk bytes.
	owner := NewOwner()
	il := getInodeLock(f, srcInoNum)
	il.LockOwner(owner)
	defer il.UnlockOwner(owner)
	srcIno, err = readInode(f, fsOffset, sb, srcInoNum)
	if err != nil {
		return err
	}
	links := binary.LittleEndian.Uint16(srcIno.raw[inodeOffLinksCount:])
	binary.LittleEndian.PutUint16(srcIno.raw[inodeOffLinksCount:], links+1)
	return writeInode(f, fsOffset, sb, srcIno)
}
