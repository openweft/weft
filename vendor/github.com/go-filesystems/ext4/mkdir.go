package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

var makeDirAllocInode = func(f readerWriterAt, fsOffset int64, sb *superblock, isDir bool) (uint32, error) {
	j := journalForAny(f)
	if j != nil && j.enabled {
		tx, terr := j.StartTx()
		if terr == nil {
			ino, cleanup, err := allocInodeWithTx(f, fsOffset, sb, isDir, tx)
			if err != nil {
				_ = tx.Abort()
				if cleanup != nil {
					cleanup()
				}
				return 0, err
			}
			if err := tx.Commit(); err != nil {
				_ = tx.Abort()
				if cleanup != nil {
					cleanup()
				}
				return 0, err
			}
			if cleanup != nil {
				cleanup()
			}
			return ino, nil
		}
	}
	return allocInode(f, fsOffset, sb, isDir)
}
var makeDirAllocBlocks = func(f readerWriterAt, fsOffset int64, sb *superblock, n uint32) ([]uint64, error) {
	j := journalForAny(f)
	if j != nil && j.enabled {
		tx, terr := j.StartTx()
		if terr == nil {
			bs, cleanup, err := allocBlocksWithTx(f, fsOffset, sb, n, tx)
			if err != nil {
				_ = tx.Abort()
				if cleanup != nil {
					cleanup()
				}
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				_ = tx.Abort()
				if cleanup != nil {
					cleanup()
				}
				return nil, err
			}
			if cleanup != nil {
				cleanup()
			}
			return bs, nil
		}
	}
	return allocBlocks(f, fsOffset, sb, n)
}

// makeDir creates a new directory at path with the given permissions.
// The parent directory must already exist. Returns an error if path already
// exists.
func makeDir(f readerWriterAt, fsOffset int64, sb *superblock, path string, perm os.FileMode) error {
	parentIno, name, err := lookupParent(f, fsOffset, sb, path)
	if err != nil {
		return err
	}

	// Serialise concurrent modifications to the same parent directory.
	dirML := getDirModLock(f, parentIno.num)
	dirML.Lock()
	defer dirML.Unlock()

	// Reject if the name already exists.
	entries, err := readDir(f, fsOffset, sb, parentIno)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name == name {
			return fmt.Errorf("ext4: %q already exists", path)
		}
	}

	// We'll allocate the inode and directory block. When a sidecar journal
	// is active we prepare both the inode and the block into a single
	// transaction so they commit atomically.
	j := journalForAny(f)
	var tx *Transaction
	var cleanupReserve func()
	var cleanupReserveIno func()
	var physBlocks []uint64
	var newInoNum uint32
	if j != nil && j.enabled {
		var terr error
		tx, terr = j.StartTx()
		if terr != nil {
			return terr
		}
		// Allocate inode into tx so reservation and metadata updates are
		// grouped with the directory block write.
		newInoNum, cleanupReserveIno, err = allocInodeWithTx(f, fsOffset, sb, true, tx)
		if err != nil {
			_ = tx.Abort()
			if cleanupReserveIno != nil {
				cleanupReserveIno()
			}
			return fmt.Errorf("ext4: alloc inode for %q: %w", path, err)
		}
		physBlocks, cleanupReserve, err = allocBlocksWithTx(f, fsOffset, sb, 1, tx)
		if err != nil {
			_ = tx.Abort()
			if cleanupReserve != nil {
				cleanupReserve()
			}
			if cleanupReserveIno != nil {
				cleanupReserveIno()
			}
			return fmt.Errorf("ext4: alloc block for %q: %w", path, err)
		}
	} else {
		newInoNum, err = makeDirAllocInode(f, fsOffset, sb, true /* isDir */)
		if err != nil {
			return fmt.Errorf("ext4: alloc inode for %q: %w", path, err)
		}
		physBlocks, err = makeDirAllocBlocks(f, fsOffset, sb, 1)
		if err != nil {
			return fmt.Errorf("ext4: alloc block for %q: %w", path, err)
		}
	}
	physBlock := physBlocks[0]

	// Build the directory data block with "." and ".." entries.
	buf := make([]byte, sb.BlockSize)
	le := binary.LittleEndian

	// "." entry — minimum size, points to the new directory's own inode.
	dotMin := minDirentSize(1) // 12 bytes
	le.PutUint32(buf[0:], newInoNum)
	le.PutUint16(buf[4:], uint16(dotMin))
	buf[6] = 1     // name_len
	buf[7] = FtDir // file_type
	buf[8] = '.'

	// ".." entry — fills the rest of the block (minus the checksum tail, if any).
	tailSize := 0
	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		tailSize = 12
	}
	ddOff := dotMin
	ddRecLen := int(sb.BlockSize) - dotMin - tailSize
	le.PutUint32(buf[ddOff:], parentIno.num)
	le.PutUint16(buf[ddOff+4:], uint16(ddRecLen))
	buf[ddOff+6] = 2     // name_len
	buf[ddOff+7] = FtDir // file_type
	buf[ddOff+8] = '.'
	buf[ddOff+9] = '.'

	// Build the new inode before writing the block so updateDirBlockCsum can
	// use its number (and zero generation).
	newIno := &inode{raw: make([]byte, sb.InodeSize), num: newInoNum}
	mode := uint16(perm&0x1FF) | 0x4000 // directory
	newIno.setMode(mode, 2)             // link count = 2: parent entry + "." self-ref
	newIno.setSize(uint64(sb.BlockSize))
	newIno.setBlocks512(sb.BlockSize / 512)
	now := uint32(time.Now().Unix())
	le.PutUint32(newIno.raw[8:], now)  // i_atime
	le.PutUint32(newIno.raw[12:], now) // i_ctime
	le.PutUint32(newIno.raw[16:], now) // i_mtime
	exts := []extentLeaf{{LogBlock: 0, PhysBlock: physBlock, Count: 1}}
	_ = newIno.setInlineExtents(exts)

	// Write the directory data block (sets checksum tail when needed). If
	// we have an active transaction, add both the block and inode into the
	// tx along with the parent directory update and parent link count bump,
	// then commit them together. Otherwise perform the direct writes.
	updateDirBlockCsum(buf, sb, newIno)
	if tx != nil {
		txAbortAndCleanup := func() {
			_ = tx.Abort()
			if cleanupReserve != nil {
				cleanupReserve()
			}
			if cleanupReserveIno != nil {
				cleanupReserveIno()
			}
		}
		// Add directory block to tx
		if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+int64(physBlock)*int64(sb.BlockSize), buf); err != nil {
			txAbortAndCleanup()
			return err
		}
		// Add inode to tx
		writeBuf := make([]byte, len(newIno.raw))
		copy(writeBuf, newIno.raw)
		off, err := inodeDiskOffset(f, fsOffset, sb, newInoNum)
		if err != nil {
			txAbortAndCleanup()
			return err
		}
		if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+off, writeBuf); err != nil {
			txAbortAndCleanup()
			return err
		}

		// Bump parent link count BEFORE journaling the parent dir entry add
		// so any inode bytes addDirEntryWithTx writes capture the new value.
		parentLinks := le.Uint16(parentIno.raw[inodeOffLinksCount:])
		le.PutUint16(parentIno.raw[inodeOffLinksCount:], parentLinks+1)

		// Journal the parent directory entry insertion (and any parent inode
		// extent/size updates) inside the same tx so the parent dir block is
		// not clobbered by journal replay on reopen.
		cleanupAddDir, err := addDirEntryWithTx(tx, f, fsOffset, sb, parentIno, newInoNum, name, FtDir)
		if err != nil {
			txAbortAndCleanup()
			return err
		}
		// Journal the parent inode (link count update) explicitly so it
		// participates in the same atomic commit as the parent dir block.
		if err := addInodeToTx(tx, f, fsOffset, sb, parentIno); err != nil {
			if cleanupAddDir != nil {
				cleanupAddDir()
			}
			txAbortAndCleanup()
			return err
		}
		if err := tx.Commit(); err != nil {
			if cleanupAddDir != nil {
				cleanupAddDir()
			}
			if cleanupReserve != nil {
				cleanupReserve()
			}
			if cleanupReserveIno != nil {
				cleanupReserveIno()
			}
			return err
		}
		if cleanupAddDir != nil {
			cleanupAddDir()
		}
		if cleanupReserve != nil {
			cleanupReserve()
		}
		if cleanupReserveIno != nil {
			cleanupReserveIno()
		}
		return nil
	}

	if err := writeRawBlock(f, fsOffset, sb, physBlock, buf); err != nil {
		return err
	}
	// Write the new directory inode.
	if err := writeInode(f, fsOffset, sb, newIno); err != nil {
		return err
	}

	// Insert entry in the parent directory.
	if err := addDirEntry(f, fsOffset, sb, parentIno, newInoNum, name, FtDir); err != nil {
		return err
	}

	// Increment parent link count: the ".." back-reference in the new dir
	// counts as an extra hard-link to the parent.
	parentLinks := le.Uint16(parentIno.raw[inodeOffLinksCount:])
	le.PutUint16(parentIno.raw[inodeOffLinksCount:], parentLinks+1)
	return writeInode(f, fsOffset, sb, parentIno)
}
