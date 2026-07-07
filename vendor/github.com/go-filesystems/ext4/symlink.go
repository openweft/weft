package filesystem_ext4

// symlink.go — symbolic-link creation (the read side lives in dir.go's
// readSymlink). ext4 stores a symlink target one of two ways, both produced
// here and already understood by readSymlink:
//
//   - Fast symlink (target ≤ 60 bytes): the target bytes live directly in the
//     60-byte i_block area, with no data blocks and the EXTENTS flag clear.
//   - Slow symlink (target > 60 bytes): the target lives in a single data
//     block addressed by an inline extent tree, exactly like a tiny regular
//     file.

import (
	"encoding/binary"
	"fmt"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// ext4FS implements the optional symlink-creation capability.
var _ filesystem.Symlinker = (*ext4FS)(nil)

// Symlink creates a symbolic link at linkPath whose target is the literal
// string `target`. The parent of linkPath must exist; linkPath must not.
func (fs *ext4FS) Symlink(target, linkPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	rw := getRW(fs)
	return makeSymlink(rw, fs.partOffset, fs.sb, target, linkPath)
}

// makeSymlink creates a symlink inode at linkPath. It mirrors makeDir's
// non-directory creation pattern: allocate an inode, build it, write it, and
// insert a typed directory entry in the parent — but with symlink mode/ftype,
// link count 1, no parent link-count bump, and target storage chosen by length.
func makeSymlink(f readerWriterAt, fsOffset int64, sb *superblock, target, linkPath string) error {
	parentIno, name, err := lookupParent(f, fsOffset, sb, linkPath)
	if err != nil {
		return err
	}

	tgt := []byte(target)
	if len(tgt) == 0 {
		return fmt.Errorf("ext4: empty symlink target")
	}
	// A symlink target lives in at most a single block (PATH_MAX is 4096 and
	// ext4 never spreads a symlink across multiple blocks).
	if len(tgt) > int(sb.BlockSize) {
		return fmt.Errorf("ext4: symlink target too long (%d > %d)", len(tgt), sb.BlockSize)
	}

	// Serialise concurrent modifications to the same parent directory and
	// refresh it so a prior writer's directory-extent updates are visible.
	dirML := getDirModLock(f, parentIno.num)
	dirML.Lock()
	defer dirML.Unlock()
	parentIno, err = readInode(f, fsOffset, sb, parentIno.num)
	if err != nil {
		return fmt.Errorf("ext4: refresh parent inode %d: %w", parentIno.num, err)
	}
	entries, err := readDir(f, fsOffset, sb, parentIno)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name == name {
			return fmt.Errorf("ext4: %q already exists", linkPath)
		}
	}

	newInoNum, err := makeDirAllocInode(f, fsOffset, sb, false /* isDir */)
	if err != nil {
		return fmt.Errorf("ext4: alloc inode for %q: %w", linkPath, err)
	}

	newIno := &inode{raw: make([]byte, sb.InodeSize), num: newInoNum}
	newIno.setMode(uint16(0o777)|0xA000, 1) // symlinks are always mode 0777, nlink 1
	newIno.setSize(uint64(len(tgt)))
	now := uint32(time.Now().Unix())
	le := binary.LittleEndian
	le.PutUint32(newIno.raw[8:], now)  // i_atime
	le.PutUint32(newIno.raw[12:], now) // i_ctime
	le.PutUint32(newIno.raw[16:], now) // i_mtime

	if len(tgt) <= 60 {
		// Fast symlink: target inline in i_block, no blocks, EXTENTS flag clear.
		copy(newIno.raw[inodeOffBlock:inodeOffBlock+len(tgt)], tgt)
		newIno.setBlocks512(0)
	} else {
		// Slow symlink: one data block addressed by an inline extent tree.
		physBlocks, aerr := makeDirAllocBlocks(f, fsOffset, sb, 1)
		if aerr != nil {
			return fmt.Errorf("ext4: alloc block for %q: %w", linkPath, aerr)
		}
		physBlock := physBlocks[0]
		buf := make([]byte, sb.BlockSize)
		copy(buf, tgt)
		if err := writeRawBlock(f, fsOffset, sb, physBlock, buf); err != nil {
			return err
		}
		if err := newIno.setInlineExtents([]extentLeaf{{LogBlock: 0, PhysBlock: physBlock, Count: 1}}); err != nil {
			return err
		}
		newIno.setBlocks512(sb.BlockSize / 512)

		// The inode is allocated before the data block, so allocInode snapshots
		// and writes the superblock while its free-block count is still one too
		// high. allocBlocks then decrements the in-memory count and writes the
		// corrected superblock, but the two superblock writes (both at the fixed
		// offset 1024) are not ordered against each other by the commit
		// dispatcher, so the stale value can land last and leave s_free_blocks
		// off by one versus the block bitmap (e2fsck: "Free blocks count
		// wrong"). Reconcile the on-disk superblock from the now-correct
		// in-memory count, mirroring delete.go's post-mutation writeSuperblock.
		if err := writeSuperblock(f, fsOffset, sb); err != nil {
			return err
		}
	}

	if err := writeInode(f, fsOffset, sb, newIno); err != nil {
		return err
	}
	return addDirEntry(f, fsOffset, sb, parentIno, newInoNum, name, FtSymlink)
}
