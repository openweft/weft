package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

var writeFileSetInlineExtents = func(in *inode, exts []extentLeaf) error {
	return in.setInlineExtents(exts)
}

// writeExtentTree writes the extent representation for the given inode.
// If the number of extents fits inline, use the inline setter. Otherwise
// allocate a single child block that contains all leaf extents and store a
// single index entry in the inode pointing to that child block (one level
// of indirection). This avoids failing when >4 extents are produced.
func writeExtentTree(f readerWriterAt, fsOffset int64, sb *superblock, in *inode, exts []extentLeaf) error {
	// Inline path when small enough.
	if len(exts) <= 4 {
		return writeFileSetInlineExtents(in, exts)
	}

	// Compute how many leaf entries fit in a data block.
	leafCap := int((int(sb.BlockSize) - 12) / 12)
	if len(exts) > leafCap {
		return fmt.Errorf("ext4: too many extents to encode (%d > %d)", len(exts), leafCap)
	}

	// Allocate one block to hold all leaf entries. If a sidecar journal is
	// active, prepare the allocation into a transaction so the child block
	// can be grouped with the subsequent write.
	j := journalForAny(f)
	var tx *Transaction
	var cleanupReserve func()
	var childBlock uint64
	if j != nil && j.enabled {
		var terr error
		tx, terr = j.StartTx()
		if terr != nil {
			return terr
		}
		bs, cleanupReserve, err := allocBlocksWithTx(f, fsOffset, sb, 1, tx)
		if err != nil {
			_ = tx.Abort()
			if cleanupReserve != nil {
				cleanupReserve()
			}
			return fmt.Errorf("ext4: alloc block for extent leafs: %w", err)
		}
		childBlock = bs[0]
		// We'll add the prepared child block to the tx after building
		// its contents below.
	} else {
		bs, err := allocBlocks(f, fsOffset, sb, 1)
		if err != nil {
			return fmt.Errorf("ext4: alloc block for extent leafs: %w", err)
		}
		childBlock = bs[0]
	}

	// Build child block buffer containing the leaf extent entries.
	childBuf := make([]byte, sb.BlockSize)
	le := binary.LittleEndian
	le.PutUint16(childBuf[0:], ExtentMagic)
	le.PutUint16(childBuf[2:], uint16(len(exts)))
	le.PutUint16(childBuf[4:], uint16(leafCap))
	le.PutUint16(childBuf[6:], 0) // depth = 0 (leaf)
	// Leaf entries.
	for i, e := range exts {
		off := 12 + i*12
		le.PutUint32(childBuf[off:], e.LogBlock)
		le.PutUint16(childBuf[off+4:], e.Count)
		le.PutUint16(childBuf[off+6:], uint16(e.PhysBlock>>32))
		le.PutUint32(childBuf[off+8:], uint32(e.PhysBlock&0xFFFFFFFF))
	}

	if tx != nil {
		off := fsOffset + int64(childBlock)*int64(sb.BlockSize)
		if err := addRangeToTx(tx, f, fsOffset, sb, off, childBuf); err != nil {
			_ = tx.Abort()
			if cleanupReserve != nil {
				cleanupReserve()
			}
			return fmt.Errorf("ext4: add child block to tx: %w", err)
		}
		if err := tx.Commit(); err != nil {
			_ = tx.Abort()
			if cleanupReserve != nil {
				cleanupReserve()
			}
			return fmt.Errorf("ext4: commit child block tx: %w", err)
		}
		if cleanupReserve != nil {
			cleanupReserve()
			cleanupReserve = nil
		}
	} else {
		if err := writeRawBlock(f, fsOffset, sb, childBlock, childBuf); err != nil {
			return fmt.Errorf("ext4: write extent leaf block %d: %w", childBlock, err)
		}
	}

	// Encode a single index entry inline in the inode that points to the
	// child block. The inode header will have depth=1 and entries=1.
	buf := in.raw[inodeOffBlock : inodeOffBlock+60]
	for i := range buf {
		buf[i] = 0
	}
	le.PutUint16(buf[0:], ExtentMagic)
	le.PutUint16(buf[2:], 1) // one index entry
	le.PutUint16(buf[4:], 4) // eh_max (inline capacity)
	le.PutUint16(buf[6:], 1) // eh_depth = 1
	le.PutUint32(buf[8:], 0) // eh_generation
	// Index entry: ee_block = logical start of child (first extent logblock)
	eeOff := 12
	le.PutUint32(buf[eeOff:], exts[0].LogBlock)
	le.PutUint32(buf[eeOff+4:], uint32(childBlock&0xFFFFFFFF))
	le.PutUint16(buf[eeOff+8:], uint16(childBlock>>32))

	// Set EXT4_EXTENTS_FL.
	flags := binary.LittleEndian.Uint32(in.raw[inodeOffFlags:])
	flags |= InodeFlagExtents
	binary.LittleEndian.PutUint32(in.raw[inodeOffFlags:], flags)

	return nil
}

// prepareExtentChild allocates a single child block for a multi-level extent
// representation and writes the leaf block to disk. This performs the heavy
// IO work outside of any inode locks so callers can update the in-memory
// inode metadata under a short critical section and then persist the inode
// (which itself will do IO outside the inode lock via writeInode).
func prepareExtentChild(f readerWriterAt, fsOffset int64, sb *superblock, exts []extentLeaf, tx *Transaction) (uint64, func(), error) {
	// Compute leaf capacity and validate.
	leafCap := int((int(sb.BlockSize) - 12) / 12)
	if len(exts) > leafCap {
		return 0, nil, fmt.Errorf("ext4: too many extents to encode (%d > %d)", len(exts), leafCap)
	}

	// Allocate one block to hold all leaf entries.
	// If a transaction is provided, ask the allocator to prepare metadata
	// into the tx and return a cleanup closure that must be invoked after
	// the caller commits. If no tx is provided but a sidecar journal is
	// available, create a local tx here so we can use the tx-aware
	// allocator and add the prepared child block into the same transaction
	// (committing it locally). This avoids TOCTOU races when callers omit
	// a tx but a journal exists for the backing file.
	var cleanup func()
	var childBlock uint64
	createdLocalTx := false
	if tx == nil {
		// Try to locate a journal for this backing file and create a
		// local transaction if found.
		j := journalForAny(f)
		if j != nil && j.enabled {
			var terr error
			tx, terr = j.StartTx()
			if terr != nil {
				return 0, nil, terr
			}
			createdLocalTx = true
		}
	}

	if tx != nil {
		bs, cln, err := allocBlocksWithTx(f, fsOffset, sb, 1, tx)
		if err != nil {
			if createdLocalTx {
				_ = tx.Abort()
			}
			if cln != nil {
				cln()
			}
			return 0, nil, fmt.Errorf("ext4: alloc block for extent leafs: %w", err)
		}
		childBlock = bs[0]
		cleanup = cln
	} else {
		bs, err := allocBlocks(f, fsOffset, sb, 1)
		if err != nil {
			return 0, nil, fmt.Errorf("ext4: alloc block for extent leafs: %w", err)
		}
		childBlock = bs[0]
	}

	// Build child block buffer containing the leaf extent entries.
	childBuf := make([]byte, sb.BlockSize)
	le := binary.LittleEndian
	le.PutUint16(childBuf[0:], ExtentMagic)
	le.PutUint16(childBuf[2:], uint16(len(exts)))
	le.PutUint16(childBuf[4:], uint16(leafCap))
	le.PutUint16(childBuf[6:], 0) // depth = 0 (leaf)
	// Leaf entries.
	for i, e := range exts {
		off := 12 + i*12
		le.PutUint32(childBuf[off:], e.LogBlock)
		le.PutUint16(childBuf[off+4:], e.Count)
		le.PutUint16(childBuf[off+6:], uint16(e.PhysBlock>>32))
		le.PutUint32(childBuf[off+8:], uint32(e.PhysBlock&0xFFFFFFFF))
	}

	// If a transaction is provided, add the prepared child block to the
	// transaction instead of writing it immediately. This lets callers
	// commit the child block and related inode/dir metadata atomically.
	if tx != nil {
		off := fsOffset + int64(childBlock)*int64(sb.BlockSize)
		if err := addRangeToTx(tx, f, fsOffset, sb, off, childBuf); err != nil {
			if createdLocalTx {
				_ = tx.Abort()
			}
			if cleanup != nil {
				cleanup()
			}
			return 0, nil, fmt.Errorf("ext4: add child block to tx: %w", err)
		}
		// If we created the tx locally, commit it here and invoke the
		// cleanup closure before returning so callers don't need to
		// manage a tx they didn't create.
		if createdLocalTx {
			if err := tx.Commit(); err != nil {
				_ = tx.Abort()
				if cleanup != nil {
					cleanup()
				}
				return 0, nil, fmt.Errorf("ext4: commit child block tx: %w", err)
			}
			if cleanup != nil {
				cleanup()
				cleanup = nil
			}
			return childBlock, nil, nil
		}
		return childBlock, cleanup, nil
	}

	if err := writeRawBlock(f, fsOffset, sb, childBlock, childBuf); err != nil {
		return 0, nil, fmt.Errorf("ext4: write extent leaf block %d: %w", childBlock, err)
	}
	return childBlock, nil, nil
}

// writeFile creates or overwrites the file at path with the supplied data.
// The parent directory must already exist.
func writeFile(f readerWriterAt, baseF readerWriterAt, pj *Journal, fsOffset int64, sb *superblock, path string, data []byte, perm os.FileMode) error {
	dirIno, name, err := lookupParent(f, fsOffset, sb, path)
	if err != nil {
		return err
	}

	debugPrintf("DEBUG writeFile start name=%q\n", path)

	// Build the inode mode: regular file + permission bits.
	mode := uint16(perm&0x1FF) | 0x8000 // regular file

	// Check if file exists — if so, reuse the inode. For create paths we keep
	// the parent directory lock held through commit to maintain a consistent
	// lock ordering (dir lock before tx data-block locks).
	var existingIno *inode
	dirML := getDirModLock(f, dirIno.num)
	dirML.Lock()
	// Refresh dirIno under dirML so directory extent updates committed by a
	// previous writer are visible before we modify directory metadata.
	freshDirIno, rerr := readInode(f, fsOffset, sb, dirIno.num)
	if rerr != nil {
		dirML.Unlock()
		return fmt.Errorf("ext4: refresh parent inode %d: %w", dirIno.num, rerr)
	}
	dirIno = freshDirIno
	entries, err := readDir(f, fsOffset, sb, dirIno)
	if err != nil {
		dirML.Unlock()
		return err
	}
	for _, e := range entries {
		if e.Name == name {
			existingIno, err = readInode(f, fsOffset, sb, e.Inode)
			if err != nil {
				dirML.Unlock()
				return err
			}
			if !existingIno.isRegular() {
				dirML.Unlock()
				return fmt.Errorf("ext4: %q exists and is not a regular file", path)
			}
			break
		}
	}
	if existingIno != nil {
		// Overwrite path does not modify parent directory entries.
		dirML.Unlock()
	}
	// Create path: dirML must be released after directory entry is committed.
	// We track this with dirMLUnlocked so we can release it early (before
	// slow post-commit verification) without double-unlocking via defer.
	dirMLUnlocked := existingIno != nil
	defer func() {
		if !dirMLUnlocked {
			dirML.Unlock()
		}
	}()

	// Number of data blocks needed.
	nBlocks := uint32((uint64(len(data)) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize))
	if nBlocks == 0 {
		nBlocks = 1 // always allocate at least one block
	}

	// Use a per-operation owner token and rely on finer-grained locks
	// (per-inode and bitmap/group locks) rather than holding a coarse
	// file-level lock which serializes all operations on the backing file.
	owner := NewOwner()

	// If the file existed, free its blocks now while holding the file lock
	// and a per-inode lock for the existing inode.
	if existingIno != nil {
		exIL := getInodeLock(f, existingIno.num)
		exIL.LockOwner(owner)
		if err := freeInodeBlocks(f, fsOffset, sb, existingIno); err != nil {
			exIL.UnlockOwner(owner)
			return err
		}
		exIL.UnlockOwner(owner)
	}

	// Allocate data blocks — try to get them contiguous. When journaling
	// we start a transaction first and let the allocator prepare the
	// bitmap/descriptor updates into the same tx to avoid separate
	// per-allocator commits that could race with the file's tx.
	var tx *Transaction
	var cleanupReserve func()
	var cleanupReserveIno func()
	var physBlocks []uint64
	// Prefer locating a journal by the stable underlying *os.File when
	// available (avoids wrapper mismatches). If that fails, fall back to
	// inspecting the provided readerWriterAt wrapper so either form
	// (direct *os.File or wrapped RW) can be mapped to a sidecar journal.
	j := pj
	if j == nil {
		if baseF != nil {
			j = journalForAny(baseF)
			if j == nil {
				j = journalForAny(f)
			}
		} else {
			j = journalForAny(f)
		}
	}
	if j == nil || !j.enabled {
		debugPrintf("DEBUG writeFile: journaling not available for fileKey=%s\n", fileLockKey(f))
	}
	if j != nil && j.enabled {
		var terr error
		tx, terr = j.StartTx()
		if terr != nil {
			return terr
		}
		physBlocks, cleanupReserve, err = allocBlocksWithTx(f, fsOffset, sb, nBlocks, tx)
		if err != nil {
			_ = tx.Abort()
			debugPrintf("DEBUG writeFile allocBlocks %q: %v\n", path, err)
			return fmt.Errorf("ext4: alloc blocks for %q: %w", path, err)
		}
		// Add each full filesystem block into the transaction as a
		// prepared range so inode-table overlaps are detected inside
		// addRangeToTx with proper canonical locking. Do NOT commit yet;
		// we'll include the inode write in the same tx later.
		for i, blkNum := range physBlocks {
			buf := make([]byte, sb.BlockSize)
			start := i * int(sb.BlockSize)
			if start < len(data) {
				end := start + int(sb.BlockSize)
				if end > len(data) {
					end = len(data)
				}
				copy(buf, data[start:end])
			}
			off := fsOffset + int64(blkNum)*int64(sb.BlockSize)
			if err := addRangeToTx(tx, f, fsOffset, sb, off, buf); err != nil {
				_ = tx.Abort()
				if cleanupReserve != nil {
					cleanupReserve()
				}
				return fmt.Errorf("ext4: add data block to tx: %w", err)
			}
		}
	} else {
		// Double-check for a sidecar journal right before allocation in
		// case registry lookups differ between wrapper types. If a journal
		// is present start a tx and prepare the allocated blocks into it so
		// later inode updates can be grouped into the same transaction.
		var j2 *Journal
		if baseF != nil {
			j2 = journalForAny(baseF)
			if j2 == nil {
				j2 = journalForAny(f)
			}
		} else {
			j2 = journalForAny(f)
		}
		if j2 != nil && j2.enabled {
			var terr error
			tx, terr = j2.StartTx()
			if terr != nil {
				return terr
			}
			physBlocks, cleanupReserve, err = allocBlocksWithTx(f, fsOffset, sb, nBlocks, tx)
			if err != nil {
				_ = tx.Abort()
				debugPrintf("DEBUG writeFile allocBlocks %q: %v\n", path, err)
				if cleanupReserve != nil {
					cleanupReserve()
				}
				return fmt.Errorf("ext4: alloc blocks for %q: %w", path, err)
			}
			// Prepare each data block into the transaction (do not commit
			// yet; the inode will be added to `tx` later and committed by
			// the common commit path below).
			for i, blkNum := range physBlocks {
				buf := make([]byte, sb.BlockSize)
				start := i * int(sb.BlockSize)
				if start < len(data) {
					end := start + int(sb.BlockSize)
					if end > len(data) {
						end = len(data)
					}
					copy(buf, data[start:end])
				}
				off := fsOffset + int64(blkNum)*int64(sb.BlockSize)
				if err := addRangeToTx(tx, f, fsOffset, sb, off, buf); err != nil {
					_ = tx.Abort()
					if cleanupReserve != nil {
						cleanupReserve()
					}
					return fmt.Errorf("ext4: add data block to tx: %w", err)
				}
			}
		} else {
			// Final fallback: attempt one more journal lookup using
			// the stable underlying file and, if found, prepare the
			// allocation into a tx so data+inode commit together.
			var j3 *Journal
			if baseF != nil {
				j3 = journalForAny(baseF)
				if j3 == nil {
					j3 = journalForAny(f)
				}
			} else {
				j3 = journalForAny(f)
			}
			if j3 != nil && j3.enabled {
				var terr error
				tx, terr = j3.StartTx()
				if terr != nil {
					return terr
				}
				physBlocks, cleanupReserve, err = allocBlocksWithTx(f, fsOffset, sb, nBlocks, tx)
				if err != nil {
					_ = tx.Abort()
					debugPrintf("DEBUG writeFile allocBlocks %q: %v\n", path, err)
					if cleanupReserve != nil {
						cleanupReserve()
					}
					return fmt.Errorf("ext4: alloc blocks for %q: %w", path, err)
				}
				// Prepare each data block into the transaction (do not commit yet).
				for i, blkNum := range physBlocks {
					buf := make([]byte, sb.BlockSize)
					start := i * int(sb.BlockSize)
					if start < len(data) {
						end := start + int(sb.BlockSize)
						if end > len(data) {
							end = len(data)
						}
						copy(buf, data[start:end])
					}
					off := fsOffset + int64(blkNum)*int64(sb.BlockSize)
					if err := addRangeToTx(tx, f, fsOffset, sb, off, buf); err != nil {
						_ = tx.Abort()
						if cleanupReserve != nil {
							cleanupReserve()
						}
						return fmt.Errorf("ext4: add data block to tx: %w", err)
					}
				}
			} else {
				// No journal available: perform plain allocation and
				// write data via writeAtWithTrace so inode-table locks
				// are acquired when necessary.
				physBlocks, err = allocBlocks(f, fsOffset, sb, nBlocks)
				if err != nil {
					debugPrintf("DEBUG writeFile allocBlocks %q: %v\n", path, err)
					return fmt.Errorf("ext4: alloc blocks for %q: %w", path, err)
				}
				for i, blkNum := range physBlocks {
					buf := make([]byte, sb.BlockSize)
					start := i * int(sb.BlockSize)
					if start < len(data) {
						end := start + int(sb.BlockSize)
						if end > len(data) {
							end = len(data)
						}
						copy(buf, data[start:end])
					}
					off := fsOffset + int64(blkNum)*int64(sb.BlockSize)
					if _, err := writeAtWithTrace(f, fsOffset, sb, off, buf); err != nil {
						debugPrintf("DEBUG writeFile writeAtWithTrace %q blk=%d: %v\n", path, blkNum, err)
						return fmt.Errorf("ext4: write block %d: %w", blkNum, err)
					}
				}
			}
		}
	}

	// Build or reuse inode. Take a per-inode lock while we modify and write
	// the inode so readers cannot observe intermediate states.
	var ino *inode
	if existingIno != nil {
		ino = existingIno
	} else {
		if tx != nil {
			var newInoNum uint32
			newInoNum, cleanupReserveIno, err = allocInodeWithTx(f, fsOffset, sb, false, tx)
			if err != nil {
				_ = tx.Abort()
				if cleanupReserve != nil {
					cleanupReserve()
				}
				if cleanupReserveIno != nil {
					cleanupReserveIno()
				}
				debugPrintf("DEBUG writeFile allocInode %q: %v\n", path, err)
				return fmt.Errorf("ext4: alloc inode for %q: %w", path, err)
			}
			ino = &inode{raw: make([]byte, sb.InodeSize), num: newInoNum}
		} else {
			// Attempt to locate a journal just before inode allocation so
			// we can prepare the inode reservation into a transaction and
			// group it with any prepared data blocks. This mirrors the
			// earlier block allocation fallbacks.
			var j2 *Journal
			if baseF != nil {
				j2 = journalForAny(baseF)
				if j2 == nil {
					j2 = journalForAny(f)
				}
			} else {
				j2 = journalForAny(f)
			}
			if j2 != nil && j2.enabled {
				var terr error
				tx2, terr := j2.StartTx()
				if terr == nil {
					nino, cln, err := allocInodeWithTx(f, fsOffset, sb, false, tx2)
					if err != nil {
						_ = tx2.Abort()
						if cln != nil {
							cln()
						}
						debugPrintf("DEBUG writeFile allocInode (local tx) %q: %v\n", path, err)
						return fmt.Errorf("ext4: alloc inode for %q: %w", path, err)
					}
					cleanupReserveIno = cln
					// Adopt the locally-created tx so subsequent inode+dir
					// additions use the same transaction and are committed
					// together in the common commit path below.
					tx = tx2
					ino = &inode{raw: make([]byte, sb.InodeSize), num: nino}
				}
			}
			if ino == nil {
				newInoNum, err := allocInode(f, fsOffset, sb, false)
				if err != nil {
					debugPrintf("DEBUG writeFile allocInode %q: %v\n", path, err)
					return fmt.Errorf("ext4: alloc inode for %q: %w", path, err)
				}
				ino = &inode{raw: make([]byte, sb.InodeSize), num: newInoNum}
			}
		}
	}

	// Build the extent tree in-memory and persist inode metadata.
	// We ensure in-memory inode updates (i_block header + timestamps)
	// are performed under the per-inode lock to avoid readers observing
	// partially-updated extent headers. Child extent blocks (leaf blocks)
	// are allocated and written before the inode header is updated so
	// the inode can point to an already-present child block.
	exts := buildExtents(physBlocks)

	// reuse `owner` created earlier for this operation
	il := getInodeLock(f, ino.num)
	var childCleanup func()

	if len(exts) <= 4 {
		// Inline extents: update the inode inline under the inode lock.
		il.LockOwner(owner)
		ino.setMode(mode, 1)
		ino.setSize(uint64(len(data)))
		sectors := nBlocks * (sb.BlockSize / 512)
		ino.setBlocks512(sectors)
		// Set timestamps to now.
		now := uint32(time.Now().Unix())
		le := binary.LittleEndian
		le.PutUint32(ino.raw[8:], now)
		le.PutUint32(ino.raw[12:], now)
		le.PutUint32(ino.raw[16:], now)
		if err := writeFileSetInlineExtents(ino, exts); err != nil {
			il.UnlockOwner(owner)
			debugPrintf("DEBUG writeFile setInlineExtents %q: %v\n", path, err)
			return fmt.Errorf("ext4: build inline extent tree for %q: %w", path, err)
		}
		il.UnlockOwner(owner)
	} else {
		// Multi-block extent: prepare child leaf block (IO) first,
		// then update the inode header atomically under the inode lock
		// to point to the child block. If a transaction `tx` is active,
		// pass it so the prepared child block is grouped with data and
		// inode bytes.
		childBlock, childCleanup, err := prepareExtentChild(f, fsOffset, sb, exts, tx)
		if err != nil {
			if tx != nil {
				_ = tx.Abort()
				if cleanupReserve != nil {
					cleanupReserve()
				}
				if cleanupReserveIno != nil {
					cleanupReserveIno()
				}
			}
			if childCleanup != nil {
				childCleanup()
			}
			debugPrintf("DEBUG writeFile prepareExtentChild %q: %v\n", path, err)
			return fmt.Errorf("ext4: build extent child for %q: %w", path, err)
		}
		il.LockOwner(owner)
		ino.setMode(mode, 1)
		ino.setSize(uint64(len(data)))
		sectors := nBlocks * (sb.BlockSize / 512)
		ino.setBlocks512(sectors)
		// Set timestamps to now.
		now := uint32(time.Now().Unix())
		le := binary.LittleEndian
		le.PutUint32(ino.raw[8:], now)
		le.PutUint32(ino.raw[12:], now)
		le.PutUint32(ino.raw[16:], now)
		// Encode a single index entry inline in the inode pointing to the child block.
		buf := ino.raw[inodeOffBlock : inodeOffBlock+60]
		for i := range buf {
			buf[i] = 0
		}
		le.PutUint16(buf[0:], ExtentMagic)
		le.PutUint16(buf[2:], 1) // one index entry
		le.PutUint16(buf[4:], 4) // eh_max (inline = 4)
		le.PutUint16(buf[6:], 1) // eh_depth = 1
		le.PutUint32(buf[8:], 0) // eh_generation
		eeOff := 12
		le.PutUint32(buf[eeOff:], exts[0].LogBlock)
		le.PutUint32(buf[eeOff+4:], uint32(childBlock&0xFFFFFFFF))
		le.PutUint16(buf[eeOff+8:], uint16(childBlock>>32))
		// Mark extents flag.
		flags := binary.LittleEndian.Uint32(ino.raw[inodeOffFlags:])
		flags |= InodeFlagExtents
		binary.LittleEndian.PutUint32(ino.raw[inodeOffFlags:], flags)
		il.UnlockOwner(owner)
	}

	// Persist the inode. If we prepared a transaction earlier (tx != nil)
	// add the inode bytes into that transaction but DO NOT commit yet.
	// The directory entry will be added into the same transaction so both
	// can be committed atomically. If no tx is present, fall back to the
	// existing writeInode path.
	if tx != nil {
		if err := addInodeToTx(tx, f, fsOffset, sb, ino); err != nil {
			_ = tx.Abort()
			if cleanupReserve != nil {
				cleanupReserve()
			}
			if cleanupReserveIno != nil {
				cleanupReserveIno()
			}
			if childCleanup != nil {
				childCleanup()
			}
			return fmt.Errorf("ext4: add inode to tx: %w", err)
		}
		// NOTE: do not commit here; directory entry will be added into the
		// same transaction below and we will commit once after that.
	} else {
		// writeInode obtains file and inode locks as needed. We attempt to
		// reuse an existing owner token when possible to avoid unnecessary
		// lock churn and to keep critical sections short.
		if err := writeInode(f, fsOffset, sb, ino); err != nil {
			debugPrintf("DEBUG writeFile writeInode %q: %v\n", path, err)
			return err
		}
	}

	// Add (or update) directory entry. When journaling, ensure the
	// directory block and parent inode are prepared into the same
	// transaction so the file data+inode+dir entry become visible atomically.
	if existingIno == nil {
		if tx != nil {
			addCleanup, err := addDirEntryWithTx(tx, f, fsOffset, sb, dirIno, ino.num, name, FtRegFile)
			if err != nil {
				debugPrintf("DEBUG writeFile addDirEntry %q: %v\n", path, err)
				_ = tx.Abort()
				if cleanupReserve != nil {
					cleanupReserve()
				}
				if cleanupReserveIno != nil {
					cleanupReserveIno()
				}
				if childCleanup != nil {
					childCleanup()
				}
				return err
			}
			// Now commit the transaction which includes data blocks,
			// inode bytes and directory entry bytes.
			// Instrumentation: record filename -> tx seq for triage.
			_ = tx.AddCommitCallback(func(seq uint64) {
				// Record the filename for this sequence so Transaction.Commit
				// can inspect applied entries and correlate block contents
				// with the filename for diagnostics.
				registerSeqName(seq, path)
				debugPrintf("DEBUG txFileCommit name=%q ino=%d seq=%d\n", path, ino.num, seq)
			})
			if err := tx.Commit(); err != nil {
				_ = tx.Abort()
				if cleanupReserve != nil {
					cleanupReserve()
				}
				if cleanupReserveIno != nil {
					cleanupReserveIno()
				}
				if childCleanup != nil {
					childCleanup()
				}
				if addCleanup != nil {
					addCleanup()
				}
				return fmt.Errorf("ext4: commit data+inode tx: %w", err)
			}

			// Release the dir mod lock as soon as the directory entry is
			// committed — post-commit verification does not need it and
			// holding it blocks concurrent writers to the same directory.
			if !dirMLUnlocked {
				dirML.Unlock()
				dirMLUnlocked = true
			}

			// Debugging: re-read on-disk inode after a successful commit and
			// report memory vs. disk flags when debug logging is enabled. This
			// helps diagnose cases where a subsequent reader observes an inode
			// without the EXTENTS flag set.
			if ext4LockDebug {
				fresh, rerr := readInode(f, fsOffset, sb, ino.num)
				if rerr != nil {
					debugPrintf("DEBUG writeFile post-commit readInode err name=%q ino=%d err=%v\n", path, ino.num, rerr)
				} else {
					le := binary.LittleEndian
					memFlags := le.Uint32(ino.raw[inodeOffFlags:])
					diskFlags := fresh.flags()
					debugPrintf("DEBUG writeFile post-commit name=%q ino=%d mem_flags=0x%08x disk_flags=0x%08x\n", path, ino.num, memFlags, diskFlags)
					if diskExts, derr := fresh.extents(f, fsOffset, sb); derr == nil {
						debugPrintf("DEBUG writeFile post-commit fileExtents name=%q ino=%d exts=%#v\n", path, ino.num, diskExts)
					} else {
						debugPrintf("DEBUG writeFile post-commit fileExtentsErr name=%q ino=%d err=%v\n", path, ino.num, derr)
					}
					// Dump a short sample of the on-disk i_block bytes for context.
					max := 60
					if max > len(fresh.raw)-inodeOffBlock {
						max = len(fresh.raw) - inodeOffBlock
					}
					if max > 0 {
						debugPrintf("DEBUG writeFile post-commit ino=%d disk i_block sample: %q\n", ino.num, string(fresh.raw[inodeOffBlock:inodeOffBlock+max]))
					}
				}
			}

			// Conservatively verify the on-disk inode visibility for journaling
			// workloads: loop briefly until the on-disk inode has the expected
			// extents flag and its extent tree parses correctly. This guards
			// against spurious read-after-write races observed in tests.
			{
				expFlags := binary.LittleEndian.Uint32(ino.raw[inodeOffFlags:])
				wantExt := expFlags & InodeFlagExtents
				if wantExt != 0 {
					ok := false
					deadline := time.Now().Add(500 * time.Millisecond)
					for !ok && time.Now().Before(deadline) {
						fresh, rerr := readInode(f, fsOffset, sb, ino.num)
						if rerr == nil {
							if fresh.flags()&InodeFlagExtents != 0 {
								if _, err := fresh.extents(f, fsOffset, sb); err == nil {
									ok = true
									break
								}
							}
						}
						// small backoff
						time.Sleep(500 * time.Microsecond)
					}
					if !ok {
						debugPrintf("WARN writeFile post-commit verification failed name=%q ino=%d exp_flags=0x%08x\n", path, ino.num, expFlags)
					}
				}
			}

			// Additional diagnostic: ensure directory entry became visible
			if tx != nil {
				if ext4LockDebug {
					dentries, derr := readDir(f, fsOffset, sb, dirIno)
					if derr != nil {
						debugPrintf("DEBUG writeFile post-commit readDir err name=%q parent=%d err=%v\n", path, dirIno.num, derr)
					} else {
						found := false
						for _, de := range dentries {
							if de.Name == name {
								found = true
								break
							}
						}
						if !found {
							// Dump small sample for context
							sample := []string{}
							for i := 0; i < len(dentries) && i < 32; i++ {
								sample = append(sample, dentries[i].Name)
							}
							debugPrintf("DEBUG writeFile post-commit MISSING_DIR_ENTRY name=%q parent=%d entries=%d sample=%q\n", path, dirIno.num, len(dentries), sample)
							if exts, xerr := dirIno.extents(f, fsOffset, sb); xerr == nil {
								debugPrintf("DEBUG writeFile post-commit parentExtents name=%q exts=%#v\n", path, exts)
								for _, e := range exts {
									for blk := uint64(0); blk < uint64(e.Count); blk++ {
										phys := e.PhysBlock + blk
										buf, berr := readRawBlock(f, fsOffset, sb, phys)
										if berr != nil {
											debugPrintf("DEBUG writeFile post-commit dirBlockReadErr name=%q physBlock=%d err=%v\n", path, phys, berr)
											continue
										}
										ents := parseDirBlock(buf)
										blockSample := []string{}
										for i := 0; i < len(ents) && i < 8; i++ {
											blockSample = append(blockSample, ents[i].Name)
										}
										debugPrintf("DEBUG writeFile post-commit dirBlock name=%q logBlock=%d physBlock=%d entries=%d sample=%q\n", path, e.LogBlock+uint32(blk), phys, len(ents), blockSample)
									}
								}
							}
						}
					}
				}

				// Conservative visibility check: wait briefly for the directory
				// entry to become observable on-disk. This guards against rare
				// ordering windows where concurrent commits may delay visible
				// directory updates. If the entry does not appear within the
				// timeout, continue (caller will observe failure).
				// NOTE: increase timeout to accommodate heavy concurrent test
				// workloads that may delay application/sync of committed
				// transactions.
				deadline := time.Now().Add(500 * time.Millisecond)
				ok := false
				for !ok && time.Now().Before(deadline) {
					if _, err := lookupPath(f, fsOffset, sb, path); err == nil {
						ok = true
						break
					}
					// small backoff
					time.Sleep(500 * time.Microsecond)
				}
			}
			if cleanupReserve != nil {
				cleanupReserve()
				cleanupReserve = nil
			}
			if cleanupReserveIno != nil {
				cleanupReserveIno()
				cleanupReserveIno = nil
			}
			if addCleanup != nil {
				addCleanup()
				addCleanup = nil
			}
			if childCleanup != nil {
				childCleanup()
				childCleanup = nil
			}
			debugPrintf("DEBUG writeFile return name=%q\n", path)
			return nil
		}
		if err := addDirEntry(f, fsOffset, sb, dirIno, ino.num, name, FtRegFile); err != nil {
			debugPrintf("DEBUG writeFile addDirEntry %q: %v\n", path, err)
			return err
		}
		debugPrintf("DEBUG writeFile return name=%q\n", path)
		return nil
	}
	if tx != nil {
		_ = tx.AddCommitCallback(func(seq uint64) {
			registerSeqName(seq, path)
			debugPrintf("DEBUG txFileCommit name=%q ino=%d seq=%d (existing)\n", path, ino.num, seq)
		})
		if err := tx.Commit(); err != nil {
			_ = tx.Abort()
			if cleanupReserve != nil {
				cleanupReserve()
			}
			if cleanupReserveIno != nil {
				cleanupReserveIno()
			}
			if childCleanup != nil {
				childCleanup()
			}
			return fmt.Errorf("ext4: commit data+inode tx (existing): %w", err)
		}
		if cleanupReserve != nil {
			cleanupReserve()
			cleanupReserve = nil
		}
		if cleanupReserveIno != nil {
			cleanupReserveIno()
			cleanupReserveIno = nil
		}
		if childCleanup != nil {
			childCleanup()
			childCleanup = nil
		}
	}
	// If file already existed, the directory entry stays (inode number unchanged).
	debugPrintf("DEBUG writeFile return name=%q (existing)\n", path)
	return nil
}

// buildExtents converts a list of physical block numbers into a slice of
// extent leaves.  Consecutive blocks are merged into one extent.
// Returns at most 4 extents (the inline limit); if more are needed the caller
// will see an error from setInlineExtents.
func buildExtents(physBlocks []uint64) []extentLeaf {
	if len(physBlocks) == 0 {
		return nil
	}
	var exts []extentLeaf
	start := physBlocks[0]
	count := uint16(1)
	logBase := uint32(0)
	for i := 1; i < len(physBlocks); i++ {
		if physBlocks[i] == physBlocks[i-1]+1 && count < 0x7FFF {
			count++
		} else {
			exts = append(exts, extentLeaf{LogBlock: logBase, PhysBlock: start, Count: count})
			logBase += uint32(count)
			start = physBlocks[i]
			count = 1
		}
	}
	exts = append(exts, extentLeaf{LogBlock: logBase, PhysBlock: start, Count: count})
	return exts
}

// freeInodeBlocks marks all data blocks of an inode as free (updates bitmaps
// and group descriptors, but NOT the superblock — the caller must do that via
// writeSuperblock if needed).
func freeInodeBlocks(f readerWriterAt, fsOffset int64, sb *superblock, in *inode) error {
	flags := in.flags()
	if flags&InodeFlagExtents == 0 {
		// Old-style block map: not supported, silently skip.
		return nil
	}
	exts, err := in.extents(f, fsOffset, sb)
	if err != nil {
		return err
	}
	for _, e := range exts {
		for blk := uint64(0); blk < uint64(e.Count); blk++ {
			phys := e.PhysBlock + blk
			if err := freeBlock(f, fsOffset, sb, phys); err != nil {
				return err
			}
		}
	}
	return nil
}
