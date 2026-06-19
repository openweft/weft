package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"strings"
)

var addDirEntryAllocBlocks = func(f readerWriterAt, fsOffset int64, sb *superblock, n uint32) ([]uint64, error) {
	// If a journal exists for this backing file, create a short-lived
	// local transaction and prepare the allocation into it so the
	// directory update can be committed atomically with the allocation.
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
var addDirEntryWriteBlock = writeRawBlock
var addDirEntrySetInlineExtents = func(in *inode, exts []extentLeaf) error {
	return in.setInlineExtents(exts)
}

// minDirentSize returns the minimum on-disk byte size for a directory entry
// with the given name length (aligned to 4 bytes).
func minDirentSize(nameLen int) int {
	return (8 + nameLen + 3) &^ 3
}

// parseDirBlock parses all directory entries from one data block.
// Entries with inode==0 or file_type==FtDirTail are skipped.
func parseDirBlock(buf []byte) []DirEntry {
	le := binary.LittleEndian
	var entries []DirEntry
	off := 0
	for off+8 <= len(buf) {
		ino := le.Uint32(buf[off:])
		recLen := int(le.Uint16(buf[off+4:]))
		nameLen := int(buf[off+6])
		fileType := buf[off+7]

		if recLen < 8 || off+recLen > len(buf) {
			break
		}
		if ino != 0 && fileType != FtDirTail && nameLen > 0 {
			name := string(buf[off+8 : off+8+nameLen])
			entries = append(entries, DirEntry{
				Inode:    ino,
				Name:     name,
				FileType: fileType,
			})
		}
		off += recLen
	}
	return entries
}

// readDir reads all entries from a directory inode.
func readDir(f readerWriterAt, fsOffset int64, sb *superblock, dirInode *inode) ([]DirEntry, error) {
	// Inline directories store their entries inside the inode (i_block +
	// system.data xattr) rather than in directory data blocks.
	if dirInode.isInline() {
		return inlineDirEntries(f, fsOffset, sb, dirInode)
	}

	exts, err := dirInode.readExtents(f, fsOffset, sb)
	if err != nil {
		return nil, err
	}
	var entries []DirEntry
	for _, e := range exts {
		for blk := uint64(0); blk < uint64(e.Count); blk++ {
			buf, err := readRawBlock(f, fsOffset, sb, e.PhysBlock+blk)
			if err != nil {
				return nil, fmt.Errorf("ext4: read dir block: %w", err)
			}
			entries = append(entries, parseDirBlock(buf)...)
		}
	}
	return entries, nil
}

// maxSymlinks bounds the number of symlinks resolved while looking up a single
// path, mirroring Linux's MAXSYMLINKS. It guards against symlink loops; it is
// deliberately unrelated to how deep the directory tree may be.
const maxSymlinks = 40

// lookupPath resolves an absolute path and returns the final inode number.
// Symlinks are followed up to maxSymlinks times.
func lookupPath(f readerWriterAt, fsOffset int64, sb *superblock, path string) (*inode, error) {
	return lookupPathFrom(f, fsOffset, sb, RootIno, path, 0)
}

// lookupPathFrom resolves path relative to startIno. symlinkHops counts only
// symlinks followed so far on this resolution; it is bounded by maxSymlinks to
// detect loops. Ordinary descent into directory components does NOT increment
// it — each descent consumes a path component and terminates on its own, so an
// arbitrarily deep but symlink-free tree resolves successfully.
func lookupPathFrom(f readerWriterAt, fsOffset int64, sb *superblock, startIno uint32, path string, symlinkHops int) (*inode, error) {
	if symlinkHops > maxSymlinks {
		return nil, fmt.Errorf("ext4: symlink loop resolving %q", path)
	}
	path = strings.TrimPrefix(path, "/")
	curIno := startIno
	if path == "" {
		// Return the start inode itself.
		return readInode(f, fsOffset, sb, curIno)
	}

	parts := strings.SplitN(path, "/", 2)
	cur, err := readInode(f, fsOffset, sb, curIno)
	if err != nil {
		return nil, err
	}
	if !cur.isDir() {
		return nil, fmt.Errorf("ext4: %q is not a directory (inode %d)", parts[0], curIno)
	}

	entries, err := readDir(f, fsOffset, sb, cur)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name == parts[0] {
			child, err := readInode(f, fsOffset, sb, e.Inode)
			if err != nil {
				return nil, err
			}
			if child.isSymlink() {
				target, err := readSymlink(f, fsOffset, sb, child)
				if err != nil {
					return nil, err
				}
				nextPath := target
				if len(parts) > 1 {
					nextPath = target + "/" + parts[1]
				}
				if strings.HasPrefix(nextPath, "/") {
					return lookupPathFrom(f, fsOffset, sb, RootIno, nextPath, symlinkHops+1)
				}
				return lookupPathFrom(f, fsOffset, sb, curIno, nextPath, symlinkHops+1)
			}
			if len(parts) == 1 {
				return child, nil
			}
			return lookupPathFrom(f, fsOffset, sb, e.Inode, parts[1], symlinkHops)
		}
	}
	// On lookup failure, emit a debug trace with current journal applySeq
	// to help correlate reader timing with transaction application.
	j := journalForAny(f)
	// Build a small sample of entry names for diagnostic context.
	var sampleNames []string
	for i := 0; i < len(entries) && i < 32; i++ {
		sampleNames = append(sampleNames, entries[i].Name)
	}
	if j != nil {
		j.mu.Lock()
		apply := j.applySeq
		next := j.nextSeq
		j.mu.Unlock()
		debugPrintf("DEBUG lookupPathFrom NOTFOUND path=%q startIno=%d component=%q applySeq=%d nextSeq=%d entries=%d sample=%q\n", path, startIno, parts[0], apply, next, len(entries), sampleNames)
	} else {
		debugPrintf("DEBUG lookupPathFrom NOTFOUND path=%q startIno=%d component=%q (no journal) entries=%d sample=%q\n", path, startIno, parts[0], len(entries), sampleNames)
	}
	return nil, fmt.Errorf("ext4: %q not found", parts[0])
}

// lookupParent resolves all path components except the last and returns the
// parent directory inode and the final filename.
func lookupParent(f readerWriterAt, fsOffset int64, sb *superblock, path string) (*inode, string, error) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", -1)
	if len(parts) == 0 || path == "" {
		return nil, "", fmt.Errorf("ext4: invalid path %q", path)
	}
	name := parts[len(parts)-1]
	if name == "" {
		return nil, "", fmt.Errorf("ext4: path must not end with /")
	}
	dirPath := strings.Join(parts[:len(parts)-1], "/")
	var dirIno *inode
	var err error
	if dirPath == "" {
		dirIno, err = readInode(f, fsOffset, sb, RootIno)
	} else {
		dirIno, err = lookupPath(f, fsOffset, sb, "/"+dirPath)
	}
	if err != nil {
		return nil, "", fmt.Errorf("ext4: parent directory %q: %w", dirPath, err)
	}
	if !dirIno.isDir() {
		return nil, "", fmt.Errorf("ext4: %q is not a directory", dirPath)
	}
	// Test-only trace: report resolved parent lookups for diagnostic correlation.
	traceLookupParentResolved(f, fsOffset, sb, "/"+dirPath+"/"+name, dirIno.num, name)
	return dirIno, name, nil
}

// readSymlink reads the target of a symlink inode.
func readSymlink(f readerWriterAt, fsOffset int64, sb *superblock, in *inode) (string, error) {
	if in.size <= 60 {
		// Fast symlink: target stored in i_block.
		return string(in.raw[inodeOffBlock : inodeOffBlock+int(in.size)]), nil
	}
	data, err := readFileData(f, fsOffset, sb, in)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// addDirEntry adds a directory entry (name → child inode) to a directory.
// It appends to an existing block or allocates a new one.
// Returns the physical block number modified (for checksum update).
func addDirEntry(f readerWriterAt, fsOffset int64, sb *superblock,
	dirIno *inode, childIno uint32, name string, fileType uint8) error {
	// Backwards-compatible wrapper: call tx-aware variant with nil tx.
	_, err := addDirEntryWithTx(nil, f, fsOffset, sb, dirIno, childIno, name, fileType)
	return err
}

// addDirEntryWithTx is a tx-aware variant of addDirEntry. If a non-nil
// Transaction is provided the function prepares the directory block and
// parent inode bytes into the supplied tx and returns a cleanup closure
// that the caller must invoke after committing the tx. If tx is nil the
// function will create and commit a local transaction when a journal is
// available, preserving the previous behavior. The returned cleanup func
// will be non-nil only when the caller passed a tx and there are cleanup
// actions (e.g., reservation cleanup) to run after commit.
func addDirEntryWithTx(tx *Transaction, f readerWriterAt, fsOffset int64, sb *superblock,
	dirIno *inode, childIno uint32, name string, fileType uint8) (func(), error) {

	needed := minDirentSize(len(name))
	exts, err := dirIno.extents(f, fsOffset, sb)
	if err != nil {
		return nil, err
	}
	// Track whether we create a local tx for journaling within this
	// call. Declare early so later branches can reuse the flag.
	createdLocalTx := false
	var cleanupReserve func()

	// Try to find space in existing blocks.
	for _, e := range exts {
		for blk := uint64(0); blk < uint64(e.Count); blk++ {
			logBlock := e.LogBlock + uint32(blk)
			if dirIno.flags()&InodeFlagHashIndex != 0 && logBlock == 0 {
				continue
			}
			physBlock := e.PhysBlock + blk
			buf, err := readRawBlock(f, fsOffset, sb, physBlock)
			if err != nil {
				return nil, err
			}
			if tryInsertDirEntry(buf, childIno, name, fileType, needed) {
				traceDirOp("add", dirIno.num, childIno, name)
				updateDirBlockCsum(buf, sb, dirIno)

				// If a transaction is present, prepare the modified directory
				// block into it so the directory update participates in the
				// same commit as inode/data. If no tx is present but a sidecar
				// journal exists, create a local tx for this operation.
				if tx != nil {
					if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+int64(physBlock)*int64(sb.BlockSize), buf); err != nil {
						if createdLocalTx {
							_ = tx.Abort()
							if cleanupReserve != nil {
								cleanupReserve()
							}
						}
						return nil, err
					}
					if createdLocalTx {
						if err := tx.Commit(); err != nil {
							if cleanupReserve != nil {
								cleanupReserve()
							}
							return nil, err
						}
						if cleanupReserve != nil {
							cleanupReserve()
							cleanupReserve = nil
						}
						return nil, nil
					}
					cleanup := func() {
						if cleanupReserve != nil {
							cleanupReserve()
						}
					}
					return cleanup, nil
				}

				if err := addDirEntryWriteBlock(f, fsOffset, sb, physBlock, buf); err != nil {
					return nil, err
				}
				return nil, nil
			}
		}
	}

	// No space — allocate a new directory block. When a sidecar journal is
	// active and no tx was supplied, create a local tx so allocations are
	// prepared into it. If the caller supplied a tx, use it and return any
	// reservation cleanup to the caller to be invoked after commit.
	var newPhys []uint64
	if tx == nil {
		// Allocate via the non-tx allocator; allocators will persist
		// metadata via the commit dispatcher when needed and will not
		// create local transactions here.
		newPhys, err = addDirEntryAllocBlocks(f, fsOffset, sb, 1)
		if err != nil {
			return nil, fmt.Errorf("ext4: alloc directory block: %w", err)
		}
	} else {
		newPhys, cleanupReserve, err = allocBlocksWithTx(f, fsOffset, sb, 1, tx)
		if err != nil {
			return nil, fmt.Errorf("ext4: alloc directory block: %w", err)
		}
	}

	buf := make([]byte, sb.BlockSize)
	// Single entry occupying the block (rec_len = blockSize - tailSize).
	writeDirEntry(buf, 0, childIno, uint16(int(sb.BlockSize)-12), name, fileType)
	updateDirBlockCsum(buf, sb, dirIno)
	if tx != nil {
		if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+int64(newPhys[0])*int64(sb.BlockSize), buf); err != nil {
			if createdLocalTx {
				_ = tx.Abort()
				if cleanupReserve != nil {
					cleanupReserve()
				}
			}
			return nil, err
		}
		// Trace the directory add for debugging.
		traceDirOp("add", dirIno.num, childIno, name)
	} else {
		if err := addDirEntryWriteBlock(f, fsOffset, sb, newPhys[0], buf); err != nil {
			return nil, err
		}
		// Trace the directory add for debugging.
		traceDirOp("add", dirIno.num, childIno, name)
	}

	// Extend the directory inode with the new block.
	logBlock := uint32(0)
	if len(exts) > 0 {
		last := exts[len(exts)-1]
		logBlock = last.LogBlock + uint32(last.Count)
	}
	exts = append(exts, extentLeaf{
		LogBlock:  logBlock,
		PhysBlock: newPhys[0],
		Count:     1,
	})

	// If extents fit inline, perform the cheap inline setter inside a
	// short critical section. If not, prepare and write the index/leaf
	// child block *before* taking the inode lock so we don't hold the
	// directory inode lock during the expensive block write.
	if len(exts) <= 4 {
		owner := NewOwner()
		il := getInodeLock(f, dirIno.num)
		il.LockOwner(owner)

		if err := addDirEntrySetInlineExtents(dirIno, exts); err != nil {
			il.UnlockOwner(owner)
			if createdLocalTx && tx != nil {
				_ = tx.Abort()
				if cleanupReserve != nil {
					cleanupReserve()
				}
			}
			return nil, err
		}
		dirIno.setSize(uint64(logBlock+1) * uint64(sb.BlockSize))
		dirIno.setBlocks512(uint32(logBlock+1) * (sb.BlockSize / 512))

		// Prepare inode bytes while holding the short critical section,
		// then release the lock before applying/adding to the transaction.
		if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
			computeInodeCsum(sb, dirIno)
		}
		traceInodeWrite(f, fsOffset, sb, dirIno.num, 0, dirIno.size)
		writeBuf := make([]byte, len(dirIno.raw))
		copy(writeBuf, dirIno.raw)
		il.UnlockOwner(owner)

		// If we started a transaction locally, add the inode range and
		// commit both the directory block and inode together. If the tx
		// was supplied by the caller, add the range to the tx and return
		// a cleanup closure for the caller to invoke after commit.
		if tx != nil {
			off, err := inodeDiskOffset(f, fsOffset, sb, dirIno.num)
			if err != nil {
				if createdLocalTx {
					_ = tx.Abort()
					if cleanupReserve != nil {
						cleanupReserve()
					}
				}
				return nil, err
			}
			if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+off, writeBuf); err != nil {
				if createdLocalTx {
					_ = tx.Abort()
					if cleanupReserve != nil {
						cleanupReserve()
					}
				}
				return nil, err
			}
			if createdLocalTx {
				if err := tx.Commit(); err != nil {
					if cleanupReserve != nil {
						cleanupReserve()
					}
					return nil, err
				}
				if cleanupReserve != nil {
					cleanupReserve()
					cleanupReserve = nil
				}
				return nil, nil
			}
			// tx was supplied by caller: return cleanup to be invoked after commit.
			cleanup := func() {
				if cleanupReserve != nil {
					cleanupReserve()
				}
			}
			return cleanup, nil
		}
		return nil, writeInode(f, fsOffset, sb, dirIno)
	}

	// Need an indirect extent: allocate+write child block outside lock.
	var childBlock uint64
	var childCleanup func()
	childBlock, childCleanup, err = prepareExtentChild(f, fsOffset, sb, exts, tx)
	if err != nil {
		if tx != nil && createdLocalTx {
			_ = tx.Abort()
			if cleanupReserve != nil {
				cleanupReserve()
			}
		}
		if childCleanup != nil {
			childCleanup()
		}
		return nil, err
	}

	// Now update inode metadata under a short lock: write an inline index
	// entry that points to the prepared child block and mark EXTENTS flag.
	owner := NewOwner()
	il := getInodeLock(f, dirIno.num)
	il.LockOwner(owner)

	// Encode a single index entry inline in the inode that points to the
	// child block. The inode header will have depth=1 and entries=1.
	inodeBuf := dirIno.raw[inodeOffBlock : inodeOffBlock+60]
	for i := range inodeBuf {
		inodeBuf[i] = 0
	}
	le := binary.LittleEndian
	le.PutUint16(inodeBuf[0:], ExtentMagic)
	le.PutUint16(inodeBuf[2:], 1) // one index entry
	le.PutUint16(inodeBuf[4:], 4) // eh_max (inline = 4)
	le.PutUint16(inodeBuf[6:], 1) // eh_depth = 1
	le.PutUint32(inodeBuf[8:], 0) // eh_generation
	// Index entry: ee_block = logical start of child (first extent logblock)
	eeOff := 12
	le.PutUint32(inodeBuf[eeOff:], exts[0].LogBlock)
	le.PutUint32(inodeBuf[eeOff+4:], uint32(childBlock&0xFFFFFFFF))
	le.PutUint16(inodeBuf[eeOff+8:], uint16(childBlock>>32))

	// Set EXT4_EXTENTS_FL.
	flags := binary.LittleEndian.Uint32(dirIno.raw[inodeOffFlags:])
	flags |= InodeFlagExtents
	binary.LittleEndian.PutUint32(dirIno.raw[inodeOffFlags:], flags)

	dirIno.setSize(uint64(logBlock+1) * uint64(sb.BlockSize))
	dirIno.setBlocks512(uint32(logBlock+1) * (sb.BlockSize / 512))

	// Short critical section: compute checksum and copy on-disk inode
	// bytes, then release the lock and either add to the transaction or
	// perform a normal write.
	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		computeInodeCsum(sb, dirIno)
	}
	traceInodeWrite(f, fsOffset, sb, dirIno.num, 0, dirIno.size)
	writeBuf := make([]byte, len(dirIno.raw))
	copy(writeBuf, dirIno.raw)
	il.UnlockOwner(owner)

	if tx != nil {
		off, err := inodeDiskOffset(f, fsOffset, sb, dirIno.num)
		if err != nil {
			if createdLocalTx {
				_ = tx.Abort()
				if childCleanup != nil {
					childCleanup()
				}
			}
			return nil, err
		}
		if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+off, writeBuf); err != nil {
			if createdLocalTx {
				_ = tx.Abort()
				if childCleanup != nil {
					childCleanup()
				}
			}
			return nil, err
		}
		if createdLocalTx {
			if err := tx.Commit(); err != nil {
				if childCleanup != nil {
					childCleanup()
				}
				return nil, err
			}
			if childCleanup != nil {
				childCleanup()
			}
			if cleanupReserve != nil {
				cleanupReserve()
				cleanupReserve = nil
			}
			return nil, nil
		}
		// tx was supplied by caller: return cleanup to be run after commit.
		cleanup := func() {
			if childCleanup != nil {
				childCleanup()
			}
			if cleanupReserve != nil {
				cleanupReserve()
			}
		}
		return cleanup, nil
	}
	return nil, writeInode(f, fsOffset, sb, dirIno)
}

// tryInsertDirEntry attempts to insert a new entry into a directory block by
// splitting the last entry's slack space. Returns true on success.
func tryInsertDirEntry(buf []byte, childIno uint32, name string, fileType uint8, needed int) bool {
	le := binary.LittleEndian
	off := 0
	for off+8 <= len(buf) {
		ino := le.Uint32(buf[off:])
		recLen := int(le.Uint16(buf[off+4:]))
		if recLen < 8 || off+recLen > len(buf) {
			break
		}
		// Stop before the checksum tail (file_type == FtDirTail).
		if buf[off+7] == FtDirTail {
			break
		}
		if ino == 0 {
			if recLen >= needed {
				writeDirEntry(buf, off, childIno, uint16(recLen), name, fileType)
				return true
			}
			off += recLen
			continue
		}

		nameLen := int(buf[off+6])
		minLen := minDirentSize(nameLen)
		if minLen > recLen {
			break
		}
		slack := recLen - minLen
		if slack >= needed {
			newOff := off + minLen
			if newOff+8 > len(buf) || newOff+slack > len(buf) || newOff+8+len(name) > len(buf) {
				return false
			}
			le.PutUint16(buf[off+4:], uint16(minLen))
			writeDirEntry(buf, newOff, childIno, uint16(slack), name, fileType)
			return true
		}
		off += recLen
	}
	return false
}

// writeDirEntry writes a raw directory entry at buf[off].
func writeDirEntry(buf []byte, off int, ino uint32, recLen uint16, name string, fileType uint8) {
	le := binary.LittleEndian
	le.PutUint32(buf[off:], ino)
	le.PutUint16(buf[off+4:], recLen)
	buf[off+6] = uint8(len(name))
	buf[off+7] = fileType
	copy(buf[off+8:], name)
}

// updateDirBlockCsum computes and writes the metadata_csum tail entry.
// If metadata_csum is not enabled, this is a no-op.
func updateDirBlockCsum(buf []byte, sb *superblock, dirIno *inode) {
	if sb.FeatureROCompat&FeatROCompatMetadataCsum == 0 {
		return
	}
	le := binary.LittleEndian
	tailOff := len(buf) - 12
	// Write tail entry header.
	le.PutUint32(buf[tailOff:], 0)    // reserved_zero
	le.PutUint16(buf[tailOff+4:], 12) // rec_len
	buf[tailOff+6] = 0                // name_len
	buf[tailOff+7] = FtDirTail        // file_type = 0xDE

	// Zero the checksum field, then compute.
	le.PutUint32(buf[tailOff+8:], 0)

	numBuf := make([]byte, 4)
	genBuf := make([]byte, 4)
	le.PutUint32(numBuf, dirIno.num)
	le.PutUint32(genBuf, dirIno.generation())

	seed := sb.csumSeed()
	csum := crc32c(seed, numBuf)
	csum = crc32c(csum, genBuf)
	csum = crc32c(csum, buf[:tailOff+8])
	le.PutUint32(buf[tailOff+8:], csum)
}
