package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var allocGroupCursor uint32
var allocCursorStride uint32 = 1

// Backoff tuning for try-lock retries. Tuned for stress runs; can be adjusted
// via environment variables for grid-search tuning.
var backoffMaxAttempts = 12

var backoffBase = 200 * time.Microsecond

// Limit concurrent allocation operations to avoid hot-spot contention
// under stress. Tunable via EXT4_MAX_CONCURRENT_ALLOC.
var maxConcurrentAlloc = 8
var allocSem chan struct{}

func init() {
	// Seed global RNG for jitter; global math/rand uses a locked source and
	// is safe for concurrent use via the package-level functions.
	rand.Seed(time.Now().UnixNano())
	// Allow overriding backoff parameters via environment for testing.
	if v := os.Getenv("EXT4_BACKOFF_MAX_ATTEMPTS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			backoffMaxAttempts = i
		}
	}
	if v := os.Getenv("EXT4_BACKOFF_BASE_US"); v != "" {
		if us, err := strconv.ParseInt(v, 10, 64); err == nil && us > 0 {
			backoffBase = time.Duration(us) * time.Microsecond
		}
	}
	if v := os.Getenv("EXT4_ALLOC_CURSOR_STRIDE"); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			allocCursorStride = uint32(i)
		}
	}
	if v := os.Getenv("EXT4_MAX_CONCURRENT_ALLOC"); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			maxConcurrentAlloc = i
		}
	}
	if maxConcurrentAlloc > 0 {
		allocSem = make(chan struct{}, maxConcurrentAlloc)
	}
}

func backoffSleep(attempt int) {
	if attempt < 0 {
		attempt = 0
	}
	// exponential base
	base := backoffBase * (1 << uint(attempt))
	if base <= 0 {
		base = backoffBase
	}
	// add randomized jitter up to `base` to reduce lock-stepping.
	jitter := time.Duration(rand.Int63n(int64(base)))
	time.Sleep(base + jitter)
}

// bitmapCsumSeed returns the CRC32c seed used for block/inode bitmap checksums
// in block group g.
func bitmapCsumSeed(sb *superblock, g uint32) uint32 {
	le := binary.LittleEndian
	gLE := make([]byte, 4)
	le.PutUint32(gLE, g)
	return crc32c(sb.csumSeed(), gLE)
}

// readBitmap reads the block or inode bitmap for block group g.
// bitmapBlock is the physical block number of the bitmap (from the BGD).
func readBitmap(f readerWriterAt, fsOffset int64, sb *superblock, bitmapBlock uint64) ([]byte, error) {
	bmap, err := readRawBlock(f, fsOffset, sb, bitmapBlock)
	if err != nil {
		return nil, err
	}
	// Attempt to merge in-memory reservations for this group+bitmap so callers
	// don't select bits that another goroutine has reserved but not yet
	// persisted. We derive the group from the bitmap block number.
	if sb != nil && sb.BlocksPerGroup != 0 {
		g, isBlock, ok := bitmapOwnerGroup(f, fsOffset, sb, bitmapBlock)
		reservedDesc := ""
		if ok {
			// Merge reserved bits (if any)
			key := bitmapLockKey(f, g, isBlock)
			reservedBitsMu.Lock()
			if rb, ok := reservedBits[key]; ok {
				// ensure lengths match before OR'ing
				n := len(bmap)
				if len(rb) < n {
					// copy into a zero-extended slice
					tmp := make([]byte, n)
					copy(tmp, rb)
					rb = tmp
				}
				for i := 0; i < n; i++ {
					bmap[i] |= rb[i]
				}
			}
			// collect reservedWant snapshot for diagnostics
			if wm, wok := reservedWant[key]; wok {
				pairs := []string{}
				count := 0
				for bit, ri := range wm {
					pairs = append(pairs, fmt.Sprintf("%d:%t", bit, ri.want))
					count++
					if count >= 8 {
						break
					}
				}
				if len(pairs) > 0 {
					reservedDesc = fmt.Sprintf("reserved=%d(%s)", len(wm), strings.Join(pairs, ","))
				}
			}
			reservedBitsMu.Unlock()
		}
		// Report diagnostics about the merged bitmap read.
		if ext4LockDebug {
			free := 0
			max := int(sb.BlocksPerGroup)
			for i := 0; i < max; i++ {
				if bmap[i/8]&(1<<uint(i%8)) == 0 {
					free++
				}
			}
			_, file, line, _ := runtime.Caller(1)
			if reservedDesc != "" {
				debugPrintf("DEBUG ReadBitmap g=%d free=%d len=%d %s caller=%s:%d\n", g, free, len(bmap), reservedDesc, file, line)
			} else {
				debugPrintf("DEBUG ReadBitmap g=%d free=%d len=%d caller=%s:%d\n", g, free, len(bmap), file, line)
			}
		}
	}
	return bmap, nil
}

func bitmapOwnerGroup(f readerWriterAt, fsOffset int64, sb *superblock, bitmapBlock uint64) (uint32, bool, bool) {
	if sb == nil {
		return 0, false, false
	}
	for g := uint32(0); g < sb.numBlockGroups(); g++ {
		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			continue
		}
		if d.BlockBitmapBlock == bitmapBlock {
			return g, true, true
		}
		if d.InodeBitmapBlock == bitmapBlock {
			return g, false, true
		}
	}
	return 0, false, false
}

// writeBitmapWithCsum writes a block or inode bitmap back, computing the
// appropriate checksum (stored in the BGD) when metadata_csum is enabled.
// The caller is responsible for updating the BGD afterwards.
func writeBitmapWithCsum(f readerWriterAt, fsOffset int64, sb *superblock, g uint32, d *bgd, isBlockBitmap bool) error {
	var bitmapBlock uint64
	if isBlockBitmap {
		bitmapBlock = d.BlockBitmapBlock
	} else {
		bitmapBlock = d.InodeBitmapBlock
	}
	bmap, err := readBitmap(f, fsOffset, sb, bitmapBlock)
	if err != nil {
		return err
	}
	return writeBitmapBuf(f, fsOffset, sb, g, d, isBlockBitmap, bitmapBlock, bmap)
}

func writeBitmapBuf(f readerWriterAt, fsOffset int64, sb *superblock, g uint32, d *bgd, isBlockBitmap bool, bitmapBlock uint64, bmap []byte) error {
	// Serialize bitmap updates for this group to avoid concurrent writers
	// stomping the same bitmap bytes. Use a per-file+group lock.
	unlock := lockBitmapGroup(f, g, isBlockBitmap)
	defer unlock()
	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		seed := bitmapCsumSeed(sb, g)
		csum := crc32c(seed, bmap)
		le := binary.LittleEndian
		if isBlockBitmap {
			le.PutUint16(d.raw[24:], uint16(csum&0xFFFF)) // bg_block_bitmap_csum_lo
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[56:], uint16(csum>>16)) // bg_block_bitmap_csum_hi
			}
		} else {
			le.PutUint16(d.raw[26:], uint16(csum&0xFFFF)) // bg_inode_bitmap_csum_lo
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[58:], uint16(csum>>16)) // bg_inode_bitmap_csum_hi
			}
		}
	}
	return writeRawBlock(f, fsOffset, sb, bitmapBlock, bmap)
}

// writeBitmapBufNoLock is like writeBitmapBuf but assumes the caller already
// holds the group bitmap lock. This avoids double-locking when higher-level
// operations need to hold the lock across read-modify-write sequences.
func writeBitmapBufNoLock(f readerWriterAt, fsOffset int64, sb *superblock, g uint32, d *bgd, isBlockBitmap bool, bitmapBlock uint64, bmap []byte) error {
	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		seed := bitmapCsumSeed(sb, g)
		csum := crc32c(seed, bmap)
		le := binary.LittleEndian
		if isBlockBitmap {
			le.PutUint16(d.raw[24:], uint16(csum&0xFFFF)) // bg_block_bitmap_csum_lo
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[56:], uint16(csum>>16)) // bg_block_bitmap_csum_hi
			}
		} else {
			le.PutUint16(d.raw[26:], uint16(csum&0xFFFF)) // bg_inode_bitmap_csum_lo
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[58:], uint16(csum>>16)) // bg_inode_bitmap_csum_hi
			}
		}
	}
	return writeRawBlock(f, fsOffset, sb, bitmapBlock, bmap)
}

// allocInode finds and allocates a free inode, updating bitmaps and
// descriptors. Returns the new inode number (1-based).
func allocInode(f readerWriterAt, fsOffset int64, sb *superblock, isDir bool) (uint32, error) {
	nGroups := sb.numBlockGroups()
	// Bound global concurrent allocInode operations to reduce lock contention.
	if allocSem != nil {
		allocSem <- struct{}{}
		defer func() { <-allocSem }()
	}
	// Start from a rotating cursor to spread group load.
	start := atomic.AddUint32(&allocGroupCursor, allocCursorStride) % nGroups
	for i := uint32(0); i < nGroups; i++ {
		g := (start + i) % nGroups
		// Fast-read descriptor first to skip full groups without taking
		// locks. When a group appears to have space, attempt to acquire
		// the per-group BGD lock then the bitmap lock in that canonical
		// order using bounded try-lock retries with small backoff to
		// reduce hot-spot queueing.
		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			return 0, err
		}
		if d.FreeInodesCount == 0 {
			continue
		}

		var unlockBGD func()
		var ok bool
		// Try to acquire per-group BGD lock with bounded retries.
		for attempt := 0; attempt < backoffMaxAttempts; attempt++ {
			unlockBGD, ok = tryLockBGDGroup(f, g)
			if ok {
				break
			}
			backoffSleep(attempt)
		}
		if !ok {
			continue
		}

		var unlock func()
		// Try to acquire bitmap lock with bounded retries; if bitmap
		// acquisition fails, release BGD and retry acquiring both.
		bitmapAcquired := false
		for attempt := 0; attempt < backoffMaxAttempts && !bitmapAcquired; attempt++ {
			unlock, ok = tryLockBitmapGroup(f, g, false)
			if ok {
				bitmapAcquired = true
				break
			}
			// release BGD while backing off
			if unlockBGD != nil {
				unlockBGD()
				unlockBGD = nil
			}
			backoffSleep(attempt)
			// try to re-acquire BGD for next bitmap attempt
			reacquired := false
			for a := 0; a < backoffMaxAttempts; a++ {
				unlockBGD, ok = tryLockBGDGroup(f, g)
				if ok {
					reacquired = true
					break
				}
				backoffSleep(a)
			}
			if !reacquired {
				break
			}
		}
		if !bitmapAcquired {
			// Under heavy contention, avoid falling back to a blocking
			// acquisition which can lead to long stalls. Record the
			// fallback for diagnostics, release any held BGD lock and
			// continue scanning other groups to reduce hotspotting.
			recordBlockingFallback("inode", f, g, false)
			if unlockBGD != nil {
				unlockBGD()
				unlockBGD = nil
			}
			continue
		}

		// Re-read descriptor/bitmap under locks and perform allocation.
		d, err = readBGD(f, fsOffset, sb, g)
		if err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return 0, err
		}
		if d.FreeInodesCount == 0 {
			unlock()
			unlockBGD()
			continue
		}
		bmap, err := readBitmap(f, fsOffset, sb, d.InodeBitmapBlock)
		if err != nil {
			unlock()
			unlockBGD()
			return 0, err
		}
		bit, ok := findFreeBit(bmap, int(sb.InodesPerGroup))
		if !ok {
			unlock()
			unlockBGD()
			continue
		}
		setBit(bmap, bit)
		d.FreeInodesCount--
		if isDir {
			d.UsedDirsCount++
		}
		// Try to group bitmap + BGD + superblock into a single journal
		// transaction when a sidecar journal is active for this backing file.
		if j := journalForAny(f); j != nil && j.enabled {
			// Avoid creating a local transaction inside allocInode; instead
			// snapshot the updated metadata and persist via the commit
			// dispatcher. This prevents isolated commits from the allocator
			// that could race with higher-level grouped operations.
			if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
				seed := bitmapCsumSeed(sb, g)
				csum := crc32c(seed, bmap)
				le := binary.LittleEndian
				// inode bitmap csum fields
				le.PutUint16(d.raw[26:], uint16(csum&0xFFFF))
				if int(sb.DescSize) >= 64 {
					le.PutUint16(d.raw[58:], uint16(csum>>16))
				}
			}
			d.encode(sb)

			atomic.AddUint32(&sb.FreeInodesCount, ^uint32(0))
			bs := int64(sb.BlockSize)
			sbBlock := uint64(1024 / bs)
			sbUnlock := lockBGDTableBlock(f, sbBlock)
			sb.encodeRaw()
			sbRawCopy := make([]byte, len(sb.raw))
			copy(sbRawCopy, sb.raw)
			sbUnlock()

			// Snapshot copies to persist via dispatcher.
			bmapCopy := make([]byte, len(bmap))
			copy(bmapCopy, bmap)
			rawCopy := make([]byte, len(d.raw))
			copy(rawCopy, d.raw)

			// Install a reservation for the chosen inode bit so other
			// allocators won't select it while we persist the change.
			reserveBitmapBits(f, sb, g, false, []int{bit}, true)
			// Release the bitmap lock so other allocators see the reservation
			// but keep the per-group BGD lock to order enqueues consistently.
			unlock()
			unlock = nil
			bitmapStart := fsOffset + int64(d.InodeBitmapBlock)*int64(sb.BlockSize)
			tableBlock := sb.bgdTableBlock()
			descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
			ops := []commitOp{
				{startAbs: bitmapStart, data: bmapCopy},
				{startAbs: fsOffset + descOff, data: rawCopy},
				{startAbs: fsOffset + 1024, data: sbRawCopy},
			}
			ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
			if err != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, false, []int{bit})
				unlockBGD()
				return 0, err
			}
			// Associate ack and sequence with reservation so unreserve waits for this task.
			associateAckWithReservation(f, g, false, []int{bit}, ack)
			associateSeqWithReservation(f, g, false, []int{bit}, seq)
			// Release per-group BGD lock after enqueueing to maintain ordering.
			unlockBGD()
			unlockBGD = nil
			// Wait for the worker to apply the enqueued writes.
			aerr := <-ack
			if ext4LockDebug {
				_, file, line, _ := runtime.Caller(0)
				debugPrintf("DEBUG ack received alloc inode group=%d ack=%p err=%v caller=%s:%d\n", g, &ack, aerr, file, line)
			}
			if aerr != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, false, []int{bit})
				return 0, aerr
			}
			// Commit succeeded; clear reservation for the allocated bit.
			unreserveBitmapBits(f, fsOffset, sb, g, false, []int{bit})
		} else {
			// Non-journal path: snapshot updated metadata, install a
			// reservation so other allocators won't pick the same inode,
			// release locks, then persist the bitmap/BGD/superblock via the
			// commit dispatcher so we don't hold metadata locks during IO.
			if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
				seed := bitmapCsumSeed(sb, g)
				csum := crc32c(seed, bmap)
				le := binary.LittleEndian
				// inode bitmap csum fields
				le.PutUint16(d.raw[26:], uint16(csum&0xFFFF))
				if int(sb.DescSize) >= 64 {
					le.PutUint16(d.raw[58:], uint16(csum>>16))
				}
			}
			d.encode(sb)
			// Update in-memory superblock counters before encoding the raw
			// bytes so the superblock written to disk reflects the new state.
			atomic.AddUint32(&sb.FreeInodesCount, ^uint32(0))
			// Snapshot copies of the blocks we will persist.
			bmapCopy := make([]byte, len(bmap))
			copy(bmapCopy, bmap)
			rawCopy := make([]byte, len(d.raw))
			copy(rawCopy, d.raw)
			// Copy superblock raw under the BGD table block lock.
			bs := int64(sb.BlockSize)
			sbBlock := uint64(1024 / bs)
			sbUnlock := lockBGDTableBlock(f, sbBlock)
			sb.encodeRaw()
			sbRawCopy := make([]byte, len(sb.raw))
			copy(sbRawCopy, sb.raw)
			sbUnlock()

			// Install a reservation for the chosen inode bit so other
			// allocators won't select it while we persist the change.
			reserveBitmapBits(f, sb, g, false, []int{bit}, true)
			// Release the bitmap lock so other allocators see the reservation
			// but keep the per-group BGD lock to order enqueues consistently.
			unlock()
			unlock = nil
			// Persist bitmap, descriptor and superblock via commit dispatcher
			// as a single batch to avoid interleaving with other operations.
			bitmapStart := fsOffset + int64(d.InodeBitmapBlock)*int64(sb.BlockSize)
			tableBlock := sb.bgdTableBlock()
			descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
			ops := []commitOp{
				{startAbs: bitmapStart, data: bmapCopy},
				{startAbs: fsOffset + descOff, data: rawCopy},
				{startAbs: fsOffset + 1024, data: sbRawCopy},
			}
			ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
			if err != nil {
				// On error, clear reservation and release BGD.
				unreserveBitmapBits(f, fsOffset, sb, g, false, []int{bit})
				unlockBGD()
				unlockBGD = nil
				return 0, err
			}
			associateAckWithReservation(f, g, false, []int{bit}, ack)
			associateSeqWithReservation(f, g, false, []int{bit}, seq)
			// Release BGD lock after enqueueing to maintain ordering.
			unlockBGD()
			unlockBGD = nil
			// Wait for the worker to apply the enqueued writes. Do this
			// after releasing the BGD lock to avoid deadlocks with the
			// worker needing the same group lock.
			aerr := <-ack
			if ext4LockDebug {
				_, file, line, _ := runtime.Caller(0)
				debugPrintf("DEBUG ack received alloc inode (nj) group=%d ack=%p err=%v caller=%s:%d\n", g, &ack, aerr, file, line)
			}
			if aerr != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, false, []int{bit})
				return 0, aerr
			}
			// Commit succeeded; clear reservation for the allocated bit.
			unreserveBitmapBits(f, fsOffset, sb, g, false, []int{bit})
		}
		if unlock != nil {
			unlock()
		}
		// Inode numbers are 1-based; within group g, first inode = g*InodesPerGroup + 1.
		ino := g*sb.InodesPerGroup + uint32(bit) + 1
		traceAllocInode(f, fsOffset, sb, ino)
		return ino, nil
	}
	return 0, fmt.Errorf("ext4: no free inodes")
}

// allocInodeWithTx behaves like allocInode but, when a non-nil `tx` is
// provided, it prepares any metadata updates into that transaction instead
// of creating and committing its own transaction. It returns a cleanup
// function that callers should invoke after the transaction is committed to
// clear in-memory reservations for the allocated inode bit. If `tx` is nil
// this function delegates to the regular behavior and returns a nil cleanup.
func allocInodeWithTx(f readerWriterAt, fsOffset int64, sb *superblock, isDir bool, tx *Transaction) (uint32, func(), error) {
	if tx == nil {
		ino, err := allocInode(f, fsOffset, sb, isDir)
		return ino, nil, err
	}
	nGroups := sb.numBlockGroups()
	if allocSem != nil {
		allocSem <- struct{}{}
		defer func() { <-allocSem }()
	}
	start := atomic.AddUint32(&allocGroupCursor, allocCursorStride) % nGroups
	for i := uint32(0); i < nGroups; i++ {
		g := (start + i) % nGroups
		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			return 0, nil, err
		}
		if d.FreeInodesCount == 0 {
			continue
		}

		var unlockBGD func()
		var ok bool
		for attempt := 0; attempt < backoffMaxAttempts; attempt++ {
			unlockBGD, ok = tryLockBGDGroup(f, g)
			if ok {
				break
			}
			backoffSleep(attempt)
		}
		if !ok {
			continue
		}

		var unlock func()
		bitmapAcquired := false
		for attempt := 0; attempt < backoffMaxAttempts && !bitmapAcquired; attempt++ {
			unlock, ok = tryLockBitmapGroup(f, g, false)
			if ok {
				bitmapAcquired = true
				break
			}
			if unlockBGD != nil {
				unlockBGD()
				unlockBGD = nil
			}
			backoffSleep(attempt)
			reacquired := false
			for a := 0; a < backoffMaxAttempts; a++ {
				unlockBGD, ok = tryLockBGDGroup(f, g)
				if ok {
					reacquired = true
					break
				}
				backoffSleep(a)
			}
			if !reacquired {
				break
			}
		}
		if !bitmapAcquired {
			recordBlockingFallback("inode", f, g, false)
			if unlockBGD != nil {
				unlockBGD()
				unlockBGD = nil
			}
			continue
		}

		d, err = readBGD(f, fsOffset, sb, g)
		if err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return 0, nil, err
		}
		if d.FreeInodesCount == 0 {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			continue
		}
		bmap, err := readBitmap(f, fsOffset, sb, d.InodeBitmapBlock)
		if err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return 0, nil, err
		}
		bit, ok := findFreeBit(bmap, int(sb.InodesPerGroup))
		if !ok {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			continue
		}
		setBit(bmap, bit)
		d.FreeInodesCount--
		if isDir {
			d.UsedDirsCount++
		}

		// Prepare descriptor/bitmap/sb into provided tx while holding
		// bitmap and BGD locks, then release locks before returning.
		if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
			seed := bitmapCsumSeed(sb, g)
			csum := crc32c(seed, bmap)
			le := binary.LittleEndian
			le.PutUint16(d.raw[26:], uint16(csum&0xFFFF))
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[58:], uint16(csum>>16))
			}
		}
		d.encode(sb)
		if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
			le := binary.LittleEndian
			le.PutUint16(d.raw[30:], 0)
			gLE := make([]byte, 4)
			le.PutUint32(gLE, g)
			csum := crc32c(sb.csumSeed(), gLE)
			csum = crc32c(csum, d.raw[:sb.DescSize])
			le.PutUint16(d.raw[30:], uint16(csum))
		}

		atomic.AddUint32(&sb.FreeInodesCount, ^uint32(0))
		bs := int64(sb.BlockSize)
		sbBlock := uint64(1024 / bs)
		sbUnlock := lockBGDTableBlock(f, sbBlock)
		sb.encodeRaw()
		sbRawCopy := make([]byte, len(sb.raw))
		copy(sbRawCopy, sb.raw)
		sbUnlock()

		bitmapOff := fsOffset + int64(d.InodeBitmapBlock)*int64(sb.BlockSize)
		if err := addRangeToTx(tx, f, fsOffset, sb, bitmapOff, bmap); err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return 0, nil, err
		}
		tableBlock := sb.bgdTableBlock()
		descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
		if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+descOff, d.raw); err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return 0, nil, err
		}
		if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+1024, sbRawCopy); err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return 0, nil, err
		}
		// Reserve chosen inode bit in-memory and release the bitmap lock.
		// Register the tx commit callback while still holding the per-group
		// BGD lock so the callback is guaranteed to be registered before
		// control returns to the caller (who may commit immediately).
		reserveBitmapBits(f, sb, g, false, []int{bit}, true)
		if unlock != nil {
			unlock()
			unlock = nil
		}

		// Build cleanup to clear reservation after caller commits.
		gcopy := g
		reserveCopy := make([]int, 1)
		reserveCopy[0] = bit
		// Register a callback so when the caller commits the provided
		// Transaction we associate the resulting sequence with the
		// reservation. Register while holding the BGD lock to avoid a
		// race where the caller commits before the callback is installed.
		_ = tx.AddCommitCallback(func(seq uint64) {
			associateSeqWithReservation(f, gcopy, false, reserveCopy, seq)
		})
		cleanup := func() { unreserveBitmapBits(f, fsOffset, sb, gcopy, false, reserveCopy) }

		// Release per-group BGD lock after registering the callback.
		unlockBGD()

		ino := g*sb.InodesPerGroup + uint32(bit) + 1
		traceAllocInode(f, fsOffset, sb, ino)
		return ino, cleanup, nil
	}
	return 0, nil, fmt.Errorf("ext4: no free inodes")
}

// allocBlocks allocates n data blocks, trying to keep them contiguous within a
// single block group. Returns the physical block numbers.
func allocBlocks(f readerWriterAt, fsOffset int64, sb *superblock, n uint32) ([]uint64, error) {
	// Diagnostic: log journal lookup for callers to help trace remaining
	// allocBlocks callsites that do not use the tx-aware allocator.
	if ext4LockDebug {
		j := journalForAny(f)
		pcs := make([]uintptr, 6)
		ncallers := runtime.Callers(2, pcs)
		frames := runtime.CallersFrames(pcs[:ncallers])
		callers := []string{}
		for i := 0; i < 3; i++ {
			if f, more := frames.Next(); more {
				callers = append(callers, fmt.Sprintf("%s %s:%d", f.Function, f.File, f.Line))
			} else {
				break
			}
		}
		debugPrintf("DEBUG allocBlocks called jFound=%t jEnabled=%t fileKey=%s callers=%v\n", j != nil, j != nil && j.enabled, fileLockKey(f), callers)
	}
	nGroups := sb.numBlockGroups()
	// Bound global concurrent allocBlocks operations to reduce lock contention.
	if allocSem != nil {
		allocSem <- struct{}{}
		defer func() { <-allocSem }()
	}
	// Start from rotating cursor to spread allocation attempts across groups.
	start := atomic.AddUint32(&allocGroupCursor, allocCursorStride) % nGroups
	for i := uint32(0); i < nGroups; i++ {
		g := (start + i) % nGroups
		// Fast-read descriptor to skip groups that don't have space.
		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			return nil, err
		}
		// Fast-path: descriptor indicates insufficient free blocks. Double-check
		// with a lightweight bitmap read (no locks) because the BGD can be
		// stale under concurrency — if the bitmap still has enough free bits
		// then consider this group a candidate and attempt to acquire locks.
		if d.FreeBlocksCount < n {
			if bmapQuick, berr := readBitmap(f, fsOffset, sb, d.BlockBitmapBlock); berr == nil {
				freeCount, _ := countFreeInBitmap(bmapQuick, int(sb.BlocksPerGroup))
				if freeCount < int(n) {
					continue
				}
				if ext4LockDebug {
					debugPrintf("DEBUG allocBlocks descriptor stale candidate group=%d freeByBitmap=%d freeByBGD=%d need=%d\n", g, freeCount, d.FreeBlocksCount, n)
				}
				// fall through and attempt to acquire locks for this group
			} else {
				// couldn't read bitmap quickly; skip this group
				continue
			}
		}

		var unlockBGD func()
		var ok bool
		// Acquire per-group BGD lock with bounded retries
		for attempt := 0; attempt < backoffMaxAttempts; attempt++ {
			unlockBGD, ok = tryLockBGDGroup(f, g)
			if ok {
				break
			}
			backoffSleep(attempt)
		}
		if !ok {
			continue
		}

		var unlock func()
		bitmapAcquired := false
		for attempt := 0; attempt < backoffMaxAttempts && !bitmapAcquired; attempt++ {
			unlock, ok = tryLockBitmapGroup(f, g, true)
			if ok {
				bitmapAcquired = true
				break
			}
			// release BGD while backing off
			if unlockBGD != nil {
				unlockBGD()
				unlockBGD = nil
			}
			backoffSleep(attempt)
			reacquired := false
			for a := 0; a < backoffMaxAttempts; a++ {
				unlockBGD, ok = tryLockBGDGroup(f, g)
				if ok {
					reacquired = true
					break
				}
				backoffSleep(a)
			}
			if !reacquired {
				break
			}
		}
		if !bitmapAcquired {
			// Under heavy contention, avoid blocking acquisition which can
			// produce long stalls. Record for diagnostics, release any held
			// BGD lock and move on to try other groups.
			recordBlockingFallback("blocks", f, g, true)
			if unlockBGD != nil {
				unlockBGD()
				unlockBGD = nil
			}
			continue
		}

		d, err = readBGD(f, fsOffset, sb, g)
		if err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return nil, err
		}
		if d.FreeBlocksCount < n {
			unlock()
			unlockBGD()
			continue
		}
		bmap, err := readBitmap(f, fsOffset, sb, d.BlockBitmapBlock)
		if err != nil {
			unlock()
			unlockBGD()
			return nil, err
		}
		if ext4LockDebug {
			_, file, line, _ := runtime.Caller(0)
			max := int(sb.BlocksPerGroup)
			freeCount := 0
			sample := []int{}
			for i := 0; i < max; i++ {
				if bmap[i/8]&(1<<uint(i%8)) == 0 {
					freeCount++
					if len(sample) < 20 {
						sample = append(sample, i)
					}
				}
			}
			debugPrintf("DEBUG allocBlocks pre-findFreeRun group=%d need=%d bmapFree=%d sampleFree=%v sbBlocksPerGroup=%d caller=%s:%d\n", g, n, freeCount, sample, sb.BlocksPerGroup, file, line)
		}
		blocks, ok := findFreeRun(bmap, int(sb.BlocksPerGroup), int(n))
		if ext4LockDebug {
			_, file, line, _ := runtime.Caller(0)
			max := int(sb.BlocksPerGroup)
			freeCount := 0
			sample := []int{}
			for i := 0; i < max; i++ {
				if bmap[i/8]&(1<<uint(i%8)) == 0 {
					freeCount++
					if len(sample) < 20 {
						sample = append(sample, i)
					}
				}
			}
			debugPrintf("DEBUG allocBlocksWithTx pre-findFreeRun group=%d need=%d bmapFree=%d sampleFree=%v sbBlocksPerGroup=%d caller=%s:%d\n", g, n, freeCount, sample, sb.BlocksPerGroup, file, line)
		}
		if !ok {
			if ext4LockDebug {
				// collect basic diagnostics to help triage fragmentation/reservations
				max := int(sb.BlocksPerGroup)
				freeCount := 0
				sample := []int{}
				for i := 0; i < max; i++ {
					if bmap[i/8]&(1<<uint(i%8)) == 0 {
						freeCount++
						if len(sample) < 20 {
							sample = append(sample, i)
						}
					}
				}
				debugPrintf("DEBUG allocBlocks findFreeRun failed group=%d need=%d bmapFree=%d sampleFree=%v sbBlocksPerGroup=%d\n", g, n, freeCount, sample, sb.BlocksPerGroup)
			}
			unlock()
			unlockBGD()
			continue
		}
		for _, b := range blocks {
			setBit(bmap, b)
		}
		// Install reservations for the chosen blocks while we still hold the
		// bitmap lock so other allocators will avoid selecting them.
		reserveList := make([]int, 0, len(blocks))
		for _, b := range blocks {
			reserveList = append(reserveList, b)
		}
		reserveBitmapBits(f, sb, g, true, reserveList, true)
		d.FreeBlocksCount -= n
		if ext4LockDebug {
			debugPrintf("DEBUG allocBlocks d.FreeBlocksCount after dec=%d sb.FreeBlocksCount=%d group=%d bits=%v\n", d.FreeBlocksCount, atomic.LoadUint64(&sb.FreeBlocksCount), g, reserveList)
		}
		if j := journalForAny(f); j != nil && j.enabled {
			// Prefer the commit-dispatcher path even when a sidecar
			// journal exists. Creating local transactions in the allocator
			// can produce isolated commits that break higher-level
			// atomicity expectations; snapshot and enqueue the metadata
			// writes instead.
			if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
				seed := bitmapCsumSeed(sb, g)
				csum := crc32c(seed, bmap)
				le := binary.LittleEndian
				le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
				if int(sb.DescSize) >= 64 {
					le.PutUint16(d.raw[56:], uint16(csum>>16))
				}
			}
			d.encode(sb)

			atomic.AddUint64(&sb.FreeBlocksCount, ^(uint64(n) - 1))
			if ext4LockDebug {
				debugPrintf("DEBUG allocBlocks sb.FreeBlocksCount after dec=%d group=%d bits=%v\n", atomic.LoadUint64(&sb.FreeBlocksCount), g, reserveList)
			}
			bmapCopy := make([]byte, len(bmap))
			copy(bmapCopy, bmap)
			rawCopy := make([]byte, len(d.raw))
			copy(rawCopy, d.raw)
			bs := int64(sb.BlockSize)
			sbBlock := uint64(1024 / bs)
			sbUnlock := lockBGDTableBlock(f, sbBlock)
			sb.encodeRaw()
			sbRawCopy := make([]byte, len(sb.raw))
			copy(sbRawCopy, sb.raw)
			sbUnlock()

			// Release the bitmap lock so other allocators see the reservation
			// but keep the per-group BGD lock to order enqueues consistently.
			if unlock != nil {
				unlock()
				unlock = nil
			}
			bitmapStart := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
			tableBlock := sb.bgdTableBlock()
			descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
			ops := []commitOp{
				{startAbs: bitmapStart, data: bmapCopy},
				{startAbs: fsOffset + descOff, data: rawCopy},
				{startAbs: fsOffset + 1024, data: sbRawCopy},
			}
			ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
			if err != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
				unlockBGD()
				return nil, err
			}
			associateAckWithReservation(f, g, true, reserveList, ack)
			associateSeqWithReservation(f, g, true, reserveList, seq)
			// Release BGD lock after enqueueing to maintain ordering.
			unlockBGD()
			// Wait for worker to apply the writes after releasing the lock.
			aerr := <-ack
			if ext4LockDebug {
				_, file, line, _ := runtime.Caller(0)
				debugPrintf("DEBUG ack received alloc blocks j-enabled group=%d ack=%p err=%v caller=%s:%d\n", g, &ack, aerr, file, line)
			}
			if aerr != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
				return nil, aerr
			}
			// Commit succeeded; clear reservations for these blocks.
			unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
		} else {
			// Non-journal path: snapshot updated metadata, install reservations
			// (already installed above), copy updated bytes, release locks,
			// and persist via commit dispatcher to avoid holding bitmap/BGD
			// locks during IO.
			if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
				seed := bitmapCsumSeed(sb, g)
				csum := crc32c(seed, bmap)
				le := binary.LittleEndian
				le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
				if int(sb.DescSize) >= 64 {
					le.PutUint16(d.raw[56:], uint16(csum>>16))
				}
			}
			d.encode(sb)
			// Update in-memory superblock counters before encoding.
			atomic.AddUint64(&sb.FreeBlocksCount, ^(uint64(n) - 1))
			if ext4LockDebug {
				debugPrintf("DEBUG allocBlocks sb.FreeBlocksCount after dec=%d group=%d bits=%v\n", atomic.LoadUint64(&sb.FreeBlocksCount), g, reserveList)
			}
			bmapCopy := make([]byte, len(bmap))
			copy(bmapCopy, bmap)
			rawCopy := make([]byte, len(d.raw))
			copy(rawCopy, d.raw)
			bs := int64(sb.BlockSize)
			sbBlock := uint64(1024 / bs)
			sbUnlock := lockBGDTableBlock(f, sbBlock)
			sb.encodeRaw()
			sbRawCopy := make([]byte, len(sb.raw))
			copy(sbRawCopy, sb.raw)
			sbUnlock()

			// Release the bitmap lock so other allocators see the reservation
			// but keep the per-group BGD lock to order enqueues consistently.
			if unlock != nil {
				unlock()
				unlock = nil
			}
			bitmapStart := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
			tableBlock := sb.bgdTableBlock()
			descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
			ops := []commitOp{
				{startAbs: bitmapStart, data: bmapCopy},
				{startAbs: fsOffset + descOff, data: rawCopy},
				{startAbs: fsOffset + 1024, data: sbRawCopy},
			}
			ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
			if err != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
				unlockBGD()
				return nil, err
			}
			associateAckWithReservation(f, g, true, reserveList, ack)
			associateSeqWithReservation(f, g, true, reserveList, seq)
			// Release BGD lock after enqueueing to maintain ordering.
			unlockBGD()
			// Wait for worker to apply the writes after releasing the lock.
			aerr := <-ack
			if ext4LockDebug {
				_, file, line, _ := runtime.Caller(0)
				debugPrintf("DEBUG ack received alloc blocks non-j group=%d ack=%p err=%v caller=%s:%d\n", g, &ack, aerr, file, line)
			}
			if aerr != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
				return nil, aerr
			}
			// Commit succeeded; clear reservations for these blocks.
			unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
		}
		if unlock != nil {
			unlock()
		}
		// Convert local block offsets to absolute block numbers.
		// Block 0 of group g is at block number: g*BlocksPerGroup + FirstDataBlock.
		groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + uint64(sb.FirstDataBlock)
		result := make([]uint64, n)
		for i, b := range blocks {
			result[i] = groupStart + uint64(b)
		}

		// Safety check: ensure none of the allocated blocks fall inside the
		// inode-table region for their group. This should not happen because
		// inode/table/bitmap bits are reserved in the bitmap, but defend
		// against unexpected descriptor races or bitmap corruption by failing
		// the allocation rather than returning blocks that could corrupt
		// metadata when written.
		first := uint64(sb.FirstDataBlock)
		for _, blk := range result {
			if blk < first || sb.BlocksPerGroup == 0 {
				continue
			}
			rel := blk - first
			g2 := uint32(rel / uint64(sb.BlocksPerGroup))
			if g2 >= sb.numBlockGroups() {
				continue
			}
			d2, derr := readBGD(f, fsOffset, sb, g2)
			if derr != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
				if unlockBGD != nil {
					unlockBGD()
				}
				return nil, derr
			}
			// Determine inode table size (in blocks) for this group and
			// fail only if the allocated block falls inside that range.
			inodeTableStart := d2.InodeTableBlock
			inodeTableBlocks := uint64(0)
			if sb.BlockSize > 0 {
				inodeTableBlocks = (uint64(sb.InodesPerGroup)*uint64(sb.InodeSize) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
			}
			if inodeTableBlocks > 0 && blk >= inodeTableStart && blk < inodeTableStart+inodeTableBlocks {
				// Unexpected allocation into inode table — fail safely.
				// Caller will handle retry/error path.
				unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
				if unlockBGD != nil {
					unlockBGD()
				}
				return nil, fmt.Errorf("ext4: allocation would allocate block %d inside inode-table group=%d", blk, g2)
			}
		}
		return result, nil
	}
	if ext4LockDebug {
		debugPrintf("DEBUG allocBlocks overall failure need=%d allocGroupCursor=%d nGroups=%d sbFree=%d\n", n, allocGroupCursor, nGroups, atomic.LoadUint64(&sb.FreeBlocksCount))
		// Dump per-group descriptor/bitmap/reservation summary to help
		// triage why allocation failed despite apparent free counts.
		for gi := uint32(0); gi < nGroups; gi++ {
			dg, derr := readBGD(f, fsOffset, sb, gi)
			if derr != nil {
				debugPrintf("DEBUG allocBlocks group=%d readBGD error=%v\n", gi, derr)
				continue
			}
			bmap, berr := readBitmap(f, fsOffset, sb, dg.BlockBitmapBlock)
			if berr != nil {
				debugPrintf("DEBUG allocBlocks group=%d readBitmap error=%v\n", gi, berr)
				continue
			}
			// compute merged free count and sample
			max := int(sb.BlocksPerGroup)
			if max > len(bmap)*8 {
				max = len(bmap) * 8
			}
			free := 0
			sample := []int{}
			for i := 0; i < max; i++ {
				if bmap[i/8]&(1<<uint(i%8)) == 0 {
					free++
					if len(sample) < 20 {
						sample = append(sample, i)
					}
				}
			}
			// capture reservedWant snapshot
			reservedBitsMu.Lock()
			var reservedCount int
			var reservedPairs []string
			if wm, ok := reservedWant[bitmapLockKey(f, gi, true)]; ok {
				reservedCount = len(wm)
				cnt := 0
				for bit, ri := range wm {
					reservedPairs = append(reservedPairs, fmt.Sprintf("%d:%t:%d", bit, ri.want, ri.seq))
					cnt++
					if cnt >= 8 {
						break
					}
				}
			}
			reservedBitsMu.Unlock()
			debugPrintf("DEBUG allocBlocks group=%d bgdFree=%d bitmapFree=%d reserved=%d sampleFree=%v reservedSample=%v\n", gi, dg.FreeBlocksCount, free, reservedCount, sample, reservedPairs)
		}
	}
	// Final conservative fallback: perform a fully-locked scan+allocate pass
	// to guard against transient descriptor/sb staleness or races. This
	// path acquires blocking per-group locks and attempts to allocate in a
	// safe, serialized manner. It should be exercised rarely.
	if res, err := lockedAllocBlocks(f, fsOffset, sb, n); err == nil {
		return res, nil
	}
	return nil, fmt.Errorf("ext4: no free blocks (need %d)", n)
}

// lockedAllocBlocks is a conservative fallback allocation that acquires
// blocking per-group locks and attempts an allocation. This is used when
// the optimistic/try-lock path fails and should be exercised rarely.
func lockedAllocBlocks(f readerWriterAt, fsOffset int64, sb *superblock, n uint32) ([]uint64, error) {
	nGroups := sb.numBlockGroups()
	for g := uint32(0); g < nGroups; g++ {
		// Conservative fallback: attempt to acquire BGD/bitmap locks using
		// non-blocking TryLock with bounded backoff so we don't create a
		// thundering herd of goroutines blocked on a single group lock.
		var unlockBGD func()
		var unlockBitmap func()
		// Try to acquire the BGD lock with bounded backoff attempts.
		acquired := false
		for attempt := 0; attempt < backoffMaxAttempts; attempt++ {
			var ok bool
			unlockBGD, ok = tryLockBGDGroup(f, g)
			if ok {
				acquired = true
				break
			}
			// brief jittered sleep before retrying
			backoffSleep(attempt)
		}
		if !acquired {
			// couldn't grab group lock without blocking; skip this group
			if ext4LockDebug {
				debugPrintf("DEBUG lockedAllocBlocks skip-group-bgd-locked group=%d\n", g)
			}
			continue
		}

		// Try to acquire the bitmap lock without blocking. If we fail, give
		// up the BGD lock and move to the next group so we don't hold the
		// BGD lock while waiting.
		acquired = false
		for attempt := 0; attempt < backoffMaxAttempts; attempt++ {
			var ok bool
			unlockBitmap, ok = tryLockBitmapGroup(f, g, true)
			if ok {
				acquired = true
				break
			}
			backoffSleep(attempt)
		}
		if !acquired {
			if unlockBGD != nil {
				unlockBGD()
			}
			if ext4LockDebug {
				debugPrintf("DEBUG lockedAllocBlocks skip-group-bitmap-locked group=%d\n", g)
			}
			continue
		}

		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			return nil, err
		}
		if d.FreeBlocksCount < n {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			continue
		}
		bmap, err := readBitmap(f, fsOffset, sb, d.BlockBitmapBlock)
		if err != nil {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			return nil, err
		}
		blocks, ok := findFreeRun(bmap, int(sb.BlocksPerGroup), int(n))
		if !ok {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			continue
		}
		for _, b := range blocks {
			setBit(bmap, b)
		}
		reserveList := make([]int, 0, len(blocks))
		for _, b := range blocks {
			reserveList = append(reserveList, b)
		}
		reserveBitmapBits(f, sb, g, true, reserveList, true)
		d.FreeBlocksCount -= n
		if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
			seed := bitmapCsumSeed(sb, g)
			csum := crc32c(seed, bmap)
			le := binary.LittleEndian
			le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[56:], uint16(csum>>16))
			}
		}
		d.encode(sb)
		atomic.AddUint64(&sb.FreeBlocksCount, ^(uint64(n) - 1))

		bmapCopy := make([]byte, len(bmap))
		copy(bmapCopy, bmap)
		rawCopy := make([]byte, len(d.raw))
		copy(rawCopy, d.raw)
		bs := int64(sb.BlockSize)
		sbBlock := uint64(1024 / bs)
		sbUnlock := lockBGDTableBlock(f, sbBlock)
		sb.encodeRaw()
		sbRawCopy := make([]byte, len(sb.raw))
		copy(sbRawCopy, sb.raw)
		sbUnlock()

		// Release bitmap lock so other allocators see the reservation,
		// but keep BGD lock to order enqueue consistently.
		if unlockBitmap != nil {
			unlockBitmap()
			unlockBitmap = nil
		}
		bitmapStart := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
		tableBlock := sb.bgdTableBlock()
		descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
		ops := []commitOp{
			{startAbs: bitmapStart, data: bmapCopy},
			{startAbs: fsOffset + descOff, data: rawCopy},
			{startAbs: fsOffset + 1024, data: sbRawCopy},
		}
		ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
		if err != nil {
			unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
			unlockBGD()
			return nil, err
		}
		associateAckWithReservation(f, g, true, reserveList, ack)
		associateSeqWithReservation(f, g, true, reserveList, seq)
		unlockBGD()
		aerr := <-ack
		if aerr != nil {
			unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
			return nil, aerr
		}
		unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
		groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + uint64(sb.FirstDataBlock)
		result := make([]uint64, n)
		for i, b := range blocks {
			result[i] = groupStart + uint64(b)
		}
		return result, nil
	}
	return nil, fmt.Errorf("ext4: no free blocks (need %d)", n)
}

// allocBlocksWithTx behaves like allocBlocks but, when a non-nil `tx` is
// provided, it prepares any metadata updates into that transaction instead
// of creating and committing its own transaction. It returns a cleanup
// function that callers should invoke after the transaction is committed to
// clear in-memory reservations for the allocated bits. If `tx` is nil this
// function delegates to the regular behavior and returns a nil cleanup.
func allocBlocksWithTx(f readerWriterAt, fsOffset int64, sb *superblock, n uint32, tx *Transaction) ([]uint64, func(), error) {
	nGroups := sb.numBlockGroups()
	// Bound global concurrent allocBlocks operations to reduce lock contention.
	if allocSem != nil {
		allocSem <- struct{}{}
		defer func() { <-allocSem }()
	}
	start := atomic.AddUint32(&allocGroupCursor, allocCursorStride) % nGroups
	for i := uint32(0); i < nGroups; i++ {
		g := (start + i) % nGroups
		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			return nil, nil, err
		}
		if d.FreeBlocksCount < n {
			if bmapQuick, berr := readBitmap(f, fsOffset, sb, d.BlockBitmapBlock); berr == nil {
				freeCount, _ := countFreeInBitmap(bmapQuick, int(sb.BlocksPerGroup))
				if freeCount < int(n) {
					continue
				}
				if ext4LockDebug {
					debugPrintf("DEBUG allocBlocksWithTx descriptor stale candidate group=%d freeByBitmap=%d freeByBGD=%d need=%d\n", g, freeCount, d.FreeBlocksCount, n)
				}
			} else {
				continue
			}
		}

		var unlockBGD func()
		var ok bool
		for attempt := 0; attempt < backoffMaxAttempts; attempt++ {
			unlockBGD, ok = tryLockBGDGroup(f, g)
			if ok {
				break
			}
			backoffSleep(attempt)
		}
		if !ok {
			continue
		}

		var unlock func()
		bitmapAcquired := false
		for attempt := 0; attempt < backoffMaxAttempts && !bitmapAcquired; attempt++ {
			unlock, ok = tryLockBitmapGroup(f, g, true)
			if ok {
				bitmapAcquired = true
				break
			}
			if unlockBGD != nil {
				unlockBGD()
				unlockBGD = nil
			}
			backoffSleep(attempt)
			reacquired := false
			for a := 0; a < backoffMaxAttempts; a++ {
				unlockBGD, ok = tryLockBGDGroup(f, g)
				if ok {
					reacquired = true
					break
				}
				backoffSleep(a)
			}
			if !reacquired {
				break
			}
		}
		if !bitmapAcquired {
			if unlockBGD != nil {
				unlockBGD()
				unlockBGD = nil
			}
			continue
		}

		d, err = readBGD(f, fsOffset, sb, g)
		if err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return nil, nil, err
		}
		if d.FreeBlocksCount < n {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			continue
		}
		bmap, err := readBitmap(f, fsOffset, sb, d.BlockBitmapBlock)
		if err != nil {
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			return nil, nil, err
		}
		blocks, ok := findFreeRunForTx(sb, g, bmap, n, tx)
		if !ok {
			if ext4LockDebug {
				max := int(sb.BlocksPerGroup)
				freeCount := 0
				sample := []int{}
				for i := 0; i < max; i++ {
					if bmap[i/8]&(1<<uint(i%8)) == 0 {
						freeCount++
						if len(sample) < 20 {
							sample = append(sample, i)
						}
					}
				}
				debugPrintf("DEBUG allocBlocksWithTx findFreeRun failed group=%d need=%d bmapFree=%d sampleFree=%v sbBlocksPerGroup=%d\n", g, n, freeCount, sample, sb.BlocksPerGroup)
			}
			if unlock != nil {
				unlock()
			}
			unlockBGD()
			continue
		}
		for _, b := range blocks {
			setBit(bmap, b)
		}
		reserveList := make([]int, 0, len(blocks))
		for _, b := range blocks {
			reserveList = append(reserveList, b)
		}
		reserveBitmapBits(f, sb, g, true, reserveList, true)
		d.FreeBlocksCount -= n

		// If the caller provided a transaction, prefer preparing updates
		// into that tx regardless of whether `journalFor(f)` would find a
		// journal instance for the wrapper `f`. Callers (like writeFile)
		// may locate the journal via the underlying *os.File and pass the
		// resulting tx here; ensure we honor that tx parameter instead of
		// falling back to a non-journal path.
		if tx != nil {
			if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
				seed := bitmapCsumSeed(sb, g)
				csum := crc32c(seed, bmap)
				le := binary.LittleEndian
				le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
				if int(sb.DescSize) >= 64 {
					le.PutUint16(d.raw[56:], uint16(csum>>16))
				}
			}
			d.encode(sb)
			atomic.AddUint64(&sb.FreeBlocksCount, ^(uint64(n) - 1))
			bs := int64(sb.BlockSize)
			sbBlock := uint64(1024 / bs)
			sbUnlock := lockBGDTableBlock(f, sbBlock)
			sb.encodeRaw()
			sbRawCopy := make([]byte, len(sb.raw))
			copy(sbRawCopy, sb.raw)
			sbUnlock()

			bitmapOff := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
			if err := addRangeToTx(tx, f, fsOffset, sb, bitmapOff, bmap); err != nil {
				if unlock != nil {
					unlock()
				}
				unlockBGD()
				return nil, nil, err
			}
			if txBitmap := tx.GetBlock(d.BlockBitmapBlock); txBitmap != nil {
				for _, bit := range reserveList {
					setBit(txBitmap, bit)
				}
				if err := tx.AddBlock(d.BlockBitmapBlock, txBitmap); err != nil {
					if unlock != nil {
						unlock()
					}
					unlockBGD()
					return nil, nil, err
				}
			}
			tableBlock := sb.bgdTableBlock()
			descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
			if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+descOff, d.raw); err != nil {
				if unlock != nil {
					unlock()
				}
				unlockBGD()
				return nil, nil, err
			}
			if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+int64(sbBlock)*int64(sb.BlockSize), sbRawCopy); err != nil {
				if unlock != nil {
					unlock()
				}
				unlockBGD()
				return nil, nil, err
			}
			// Release bitmap lock; we purposely do NOT clear
			// reservations or commit here — caller will commit the
			// provided tx and should call the returned cleanup.
			if unlock != nil {
				unlock()
				unlock = nil
			}
			// Compute absolute block numbers to return.
			groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + uint64(sb.FirstDataBlock)
			result := make([]uint64, n)
			for i, b := range blocks {
				result[i] = groupStart + uint64(b)
			}
			// Build cleanup closure to clear reservations after commit and
			// register a tx callback to associate the eventual seq.
			gcopy := g
			reserveCopy := make([]int, len(reserveList))
			copy(reserveCopy, reserveList)
			_ = tx.AddCommitCallback(func(seq uint64) {
				associateSeqWithReservation(f, gcopy, true, reserveCopy, seq)
			})
			cleanup := func() { unreserveBitmapBits(f, fsOffset, sb, gcopy, true, reserveCopy) }
			unlockBGD()
			return result, cleanup, nil
		}
		if j := journalForAny(f); j != nil && j.enabled {
			// If caller provided a transaction, prepare descriptor+bitmap
			// updates into that tx and return a cleanup closure that will
			// clear reservations after the caller commits. Otherwise,
			// create a local tx and commit as before.
			// Local journaling path (no tx provided): behave as before.
			// If a sidecar journal is present, prefer persisting changes via the
			// commit dispatcher rather than creating a local transaction here.
			// Creating local transactions from allocators can produce isolated
			// commits that make data visible before higher-level metadata is
			// grouped; use the same non-journal commit dispatcher path to keep
			// ordering consistent without issuing an independent tx.
			if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
				seed := bitmapCsumSeed(sb, g)
				csum := crc32c(seed, bmap)
				le := binary.LittleEndian
				le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
				if int(sb.DescSize) >= 64 {
					le.PutUint16(d.raw[56:], uint16(csum>>16))
				}
			}
			d.encode(sb)
			if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
				le := binary.LittleEndian
				le.PutUint16(d.raw[30:], 0)
				gLE := make([]byte, 4)
				le.PutUint32(gLE, g)
				csum := crc32c(sb.csumSeed(), gLE)
				csum = crc32c(csum, d.raw[:sb.DescSize])
				le.PutUint16(d.raw[30:], uint16(csum))
			}

			atomic.AddUint64(&sb.FreeBlocksCount, ^(uint64(n) - 1))
			bmapCopy := make([]byte, len(bmap))
			copy(bmapCopy, bmap)
			rawCopy := make([]byte, len(d.raw))
			copy(rawCopy, d.raw)
			bs := int64(sb.BlockSize)
			sbBlock := uint64(1024 / bs)
			sbUnlock := lockBGDTableBlock(f, sbBlock)
			sb.encodeRaw()
			sbRawCopy := make([]byte, len(sb.raw))
			copy(sbRawCopy, sb.raw)
			sbUnlock()

			if unlock != nil {
				unlock()
				unlock = nil
			}
			bitmapStart := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
			tableBlock := sb.bgdTableBlock()
			descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
			ops := []commitOp{
				{startAbs: bitmapStart, data: bmapCopy},
				{startAbs: fsOffset + descOff, data: rawCopy},
				{startAbs: fsOffset + 1024, data: sbRawCopy},
			}
			ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
			if err != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
				unlockBGD()
				return nil, nil, err
			}
			associateAckWithReservation(f, g, true, reserveList, ack)
			associateSeqWithReservation(f, g, true, reserveList, seq)
			// Release BGD lock after enqueueing to maintain ordering.
			unlockBGD()
			// Wait for worker to apply the writes after releasing the lock.
			aerr := <-ack
			if ext4LockDebug {
				_, file, line, _ := runtime.Caller(0)
				debugPrintf("DEBUG ack received allocBlocksWithTx (journal) group=%d ack=%p err=%v caller=%s:%d\n", g, &ack, aerr, file, line)
			}
			if aerr != nil {
				unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
				return nil, nil, aerr
			}
			// Commit succeeded; clear reservations for these blocks.
			unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
			// Compute absolute block numbers to return.
			groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + uint64(sb.FirstDataBlock)
			result := make([]uint64, n)
			for i, b := range blocks {
				result[i] = groupStart + uint64(b)
			}
			if unlock != nil {
				unlock()
			}
			return result, nil, nil
		}

		// Non-journal path: snapshot updated metadata, install reservations
		// (already installed above), copy updated bytes, release locks,
		// and persist via commit dispatcher to avoid holding bitmap/BGD
		// locks during IO.
		if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
			seed := bitmapCsumSeed(sb, g)
			csum := crc32c(seed, bmap)
			le := binary.LittleEndian
			le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[56:], uint16(csum>>16))
			}
		}
		d.encode(sb)
		atomic.AddUint64(&sb.FreeBlocksCount, ^(uint64(n) - 1))
		bmapCopy := make([]byte, len(bmap))
		copy(bmapCopy, bmap)
		rawCopy := make([]byte, len(d.raw))
		copy(rawCopy, d.raw)
		bs := int64(sb.BlockSize)
		sbBlock := uint64(1024 / bs)
		sbUnlock := lockBGDTableBlock(f, sbBlock)
		sb.encodeRaw()
		sbRawCopy := make([]byte, len(sb.raw))
		copy(sbRawCopy, sb.raw)
		sbUnlock()

		if unlock != nil {
			unlock()
			unlock = nil
		}
		bitmapStart := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
		tableBlock := sb.bgdTableBlock()
		descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
		ops := []commitOp{
			{startAbs: bitmapStart, data: bmapCopy},
			{startAbs: fsOffset + descOff, data: rawCopy},
			{startAbs: fsOffset + 1024, data: sbRawCopy},
		}
		ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
		if err != nil {
			unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
			unlockBGD()
			return nil, nil, err
		}
		associateAckWithReservation(f, g, true, reserveList, ack)
		associateSeqWithReservation(f, g, true, reserveList, seq)
		// Release BGD lock after enqueueing to maintain ordering.
		unlockBGD()
		// Wait for worker to apply the writes after releasing the lock.
		aerr := <-ack
		if ext4LockDebug {
			_, file, line, _ := runtime.Caller(0)
			debugPrintf("DEBUG ack received allocBlocksWithTx (non-journal) group=%d ack=%p err=%v caller=%s:%d\n", g, &ack, aerr, file, line)
		}
		if aerr != nil {
			unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
			return nil, nil, aerr
		}
		// Commit succeeded; clear reservations for these blocks.
		unreserveBitmapBits(f, fsOffset, sb, g, true, reserveList)
		groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + uint64(sb.FirstDataBlock)
		result := make([]uint64, n)
		for i, b := range blocks {
			result[i] = groupStart + uint64(b)
		}
		return result, nil, nil
	}
	if ext4LockDebug {
		debugPrintf("DEBUG allocBlocksWithTx overall failure need=%d allocGroupCursor=%d nGroups=%d sbFree=%d\n", n, allocGroupCursor, nGroups, atomic.LoadUint64(&sb.FreeBlocksCount))
	}
	// Conservative fallback: try a fully-locked prepare into the provided
	// transaction (if any) or into the dispatcher when tx==nil.
	if tx == nil {
		if res, err := lockedAllocBlocks(f, fsOffset, sb, n); err == nil {
			return res, nil, nil
		}
	} else {
		if res, cleanup, err := lockedAllocBlocksWithTx(f, fsOffset, sb, n, tx); err == nil {
			return res, cleanup, nil
		}
	}
	return nil, nil, fmt.Errorf("ext4: no free blocks (need %d)", n)
}

func findFreeRunForTx(sb *superblock, g uint32, bmap []byte, n uint32, tx *Transaction) ([]int, bool) {
	if tx == nil {
		return findFreeRun(bmap, int(sb.BlocksPerGroup), int(n))
	}
	groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + uint64(sb.FirstDataBlock)
	searchMap := append([]byte(nil), bmap...)
	for {
		blocks, ok := findFreeRun(searchMap, int(sb.BlocksPerGroup), int(n))
		if !ok {
			return nil, false
		}
		conflict := false
		for _, bit := range blocks {
			if tx.GetBlock(groupStart+uint64(bit)) != nil {
				setBit(searchMap, bit)
				conflict = true
			}
		}
		if !conflict {
			return blocks, true
		}
	}
}

// lockedAllocBlocksWithTx is the tx-aware variant of the fully-locked
// fallback allocator. When a non-nil tx is provided, this prepares the
// bitmap/descriptor/superblock updates into the tx and returns a cleanup
// function that should be invoked after the caller commits the tx to
// clear in-memory reservations.
func lockedAllocBlocksWithTx(f readerWriterAt, fsOffset int64, sb *superblock, n uint32, tx *Transaction) ([]uint64, func(), error) {
	nGroups := sb.numBlockGroups()
	for g := uint32(0); g < nGroups; g++ {
		// Attempt to acquire BGD/bitmap locks using non-blocking TryLock with
		// bounded backoff to avoid creating long blocking queues under
		// contention. If we cannot acquire both locks within the bounded
		// attempts, skip this group.
		var unlockBGD func()
		var unlockBitmap func()
		acquired := false
		for attempt := 0; attempt < backoffMaxAttempts; attempt++ {
			var ok bool
			unlockBGD, ok = tryLockBGDGroup(f, g)
			if ok {
				acquired = true
				break
			}
			backoffSleep(attempt)
		}
		if !acquired {
			if ext4LockDebug {
				debugPrintf("DEBUG lockedAllocBlocksWithTx skip-group-bgd-locked group=%d\n", g)
			}
			continue
		}
		acquired = false
		for attempt := 0; attempt < backoffMaxAttempts; attempt++ {
			var ok bool
			unlockBitmap, ok = tryLockBitmapGroup(f, g, true)
			if ok {
				acquired = true
				break
			}
			backoffSleep(attempt)
		}
		if !acquired {
			if unlockBGD != nil {
				unlockBGD()
			}
			if ext4LockDebug {
				debugPrintf("DEBUG lockedAllocBlocksWithTx skip-group-bitmap-locked group=%d\n", g)
			}
			continue
		}

		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			return nil, nil, err
		}
		if d.FreeBlocksCount < n {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			continue
		}
		bmap, err := readBitmap(f, fsOffset, sb, d.BlockBitmapBlock)
		if err != nil {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			return nil, nil, err
		}
		blocks, ok := findFreeRun(bmap, int(sb.BlocksPerGroup), int(n))
		if !ok {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			continue
		}
		for _, b := range blocks {
			setBit(bmap, b)
		}
		reserveList := make([]int, 0, len(blocks))
		for _, b := range blocks {
			reserveList = append(reserveList, b)
		}
		reserveBitmapBits(f, sb, g, true, reserveList, true)
		d.FreeBlocksCount -= n
		if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
			seed := bitmapCsumSeed(sb, g)
			csum := crc32c(seed, bmap)
			le := binary.LittleEndian
			le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[56:], uint16(csum>>16))
			}
		}
		d.encode(sb)
		atomic.AddUint64(&sb.FreeBlocksCount, ^(uint64(n) - 1))

		// Prepare ranges into tx while still holding locks to avoid races
		// where the caller might commit before callbacks are installed.
		bitmapOff := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
		if err := addRangeToTx(tx, f, fsOffset, sb, bitmapOff, bmap); err != nil {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			return nil, nil, err
		}
		tableBlock := sb.bgdTableBlock()
		descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
		if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+descOff, d.raw); err != nil {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			return nil, nil, err
		}
		bs := int64(sb.BlockSize)
		sbBlock := uint64(1024 / bs)
		sbUnlock := lockBGDTableBlock(f, sbBlock)
		sb.encodeRaw()
		sbRawCopy := make([]byte, len(sb.raw))
		copy(sbRawCopy, sb.raw)
		sbUnlock()
		if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+int64(sbBlock)*int64(sb.BlockSize), sbRawCopy); err != nil {
			if unlockBitmap != nil {
				unlockBitmap()
			}
			unlockBGD()
			return nil, nil, err
		}

		// Release bitmap lock so other allocators see reservation but keep
		// BGD lock while installing tx callback.
		if unlockBitmap != nil {
			unlockBitmap()
			unlockBitmap = nil
		}

		// Register tx callback while holding BGD to avoid a race where the
		// caller commits before the callback is installed.
		gcopy := g
		reserveCopy := make([]int, len(reserveList))
		copy(reserveCopy, reserveList)
		_ = tx.AddCommitCallback(func(seq uint64) {
			associateSeqWithReservation(f, gcopy, true, reserveCopy, seq)
		})
		cleanup := func() { unreserveBitmapBits(f, fsOffset, sb, gcopy, true, reserveCopy) }

		// Release BGD lock and return result + cleanup for caller to call after commit.
		unlockBGD()
		groupStart := uint64(g)*uint64(sb.BlocksPerGroup) + uint64(sb.FirstDataBlock)
		result := make([]uint64, n)
		for i, b := range blocks {
			result[i] = groupStart + uint64(b)
		}
		return result, cleanup, nil
	}
	return nil, nil, fmt.Errorf("ext4: no free blocks (need %d)", n)
}

// freeBlock marks a single block as free in its block group.
func freeBlock(f readerWriterAt, fsOffset int64, sb *superblock, physBlock uint64) error {
	first := uint64(sb.FirstDataBlock)
	if physBlock < first {
		return nil // metadata block — don't touch
	}
	rel := physBlock - first
	g := uint32(rel / uint64(sb.BlocksPerGroup))
	bit := int(rel % uint64(sb.BlocksPerGroup))
	// Acquire locks in canonical order: per-group BGD -> bitmap.
	// This avoids lock-order inversions with allocation paths that take
	// the BGD lock before the bitmap lock.
	unlockBGD := lockBGDGroup(f, g)
	// Acquire bitmap lock while holding BGD to maintain canonical order.
	unlockBitmap := lockBitmapGroup(f, g, true)

	// Ensure we release locks on all error paths.
	defer func() {
		// If bitmap still held, release it. unlockBGD may already have
		// been released in the journaling fast-path below.
		if unlockBitmap != nil {
			unlockBitmap()
		}
	}()

	d, err := readBGD(f, fsOffset, sb, g)
	if err != nil {
		unlockBGD()
		return err
	}
	bmap, err := readBitmap(f, fsOffset, sb, d.BlockBitmapBlock)
	if err != nil {
		unlockBGD()
		return err
	}
	clearBit(bmap, bit)
	d.FreeBlocksCount++
	if ext4LockDebug {
		debugPrintf("DEBUG freeBlock d.FreeBlocksCount after inc=%d sb.FreeBlocksCount(before)=%d group=%d bit=%d\n", d.FreeBlocksCount, atomic.LoadUint64(&sb.FreeBlocksCount), g, bit)
	}

	// Journaling path: instead of creating and committing a local Transaction
	// here (which can produce isolated commits visible before higher-level
	// callers commit), snapshot the updated metadata and persist via the
	// commit dispatcher. The dispatcher applies direct writes (no per-op
	// transactions) which preserves ordering without creating intermediate
	// journal commits that break caller atomicity.
	if j := journalForAny(f); j != nil && j.enabled {
		if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
			seed := bitmapCsumSeed(sb, g)
			csum := crc32c(seed, bmap)
			le := binary.LittleEndian
			le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
			if int(sb.DescSize) >= 64 {
				le.PutUint16(d.raw[56:], uint16(csum>>16))
			}
		}
		d.encode(sb)
		if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
			le := binary.LittleEndian
			le.PutUint16(d.raw[30:], 0)
			gLE := make([]byte, 4)
			le.PutUint32(gLE, g)
			csum := crc32c(sb.csumSeed(), gLE)
			csum = crc32c(csum, d.raw[:sb.DescSize])
			le.PutUint16(d.raw[30:], uint16(csum))
		}

		atomic.AddUint64(&sb.FreeBlocksCount, 1)
		if ext4LockDebug {
			debugPrintf("DEBUG freeBlock sb.FreeBlocksCount after inc=%d group=%d bit=%d\n", atomic.LoadUint64(&sb.FreeBlocksCount), g, bit)
		}
		// Encode superblock under the BGD table block lock and copy out the
		// bytes so we can persist them via the dispatcher without holding the lock.
		bs := int64(sb.BlockSize)
		sbBlock := uint64(1024 / bs)
		sbUnlock := lockBGDTableBlock(f, sbBlock)
		sb.encodeRaw()
		sbRawCopy := make([]byte, len(sb.raw))
		copy(sbRawCopy, sb.raw)
		sbUnlock()

		// Snapshot buffers to persist via commit dispatcher.
		bmapCopy := make([]byte, len(bmap))
		copy(bmapCopy, bmap)
		rawCopy := make([]byte, len(d.raw))
		copy(rawCopy, d.raw)

		bitmapStart := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
		tableBlock := sb.bgdTableBlock()
		descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
		ops := []commitOp{
			{startAbs: bitmapStart, data: bmapCopy},
			{startAbs: fsOffset + descOff, data: rawCopy},
			{startAbs: fsOffset + 1024, data: sbRawCopy},
		}
		// Reserve the cleared bit so allocators won't pick it before we
		// persist the change, then enqueue the direct apply while still
		// holding the per-group BGD lock. Associating the ack/seq with the
		// reservation while holding the BGD lock avoids a TOCTOU where an
		// unreserve could run before the association is recorded.
		reserveBitmapBits(f, sb, g, true, []int{bit}, false)
		ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
		if err != nil {
			unreserveBitmapBits(f, fsOffset, sb, g, true, []int{bit})
			return err
		}
		associateAckWithReservation(f, g, true, []int{bit}, ack)
		associateSeqWithReservation(f, g, true, []int{bit}, seq)
		// Release the per-group locks after recording the association so
		// unreserve can reliably observe the ack/seq and wait on it.
		unlockBGD()
		unlockBGD = nil
		unlockBitmap()
		unlockBitmap = nil
		aerr := <-ack
		if ext4LockDebug {
			_, file, line, _ := runtime.Caller(0)
			debugPrintf("DEBUG ack received freeBlock (journal) group=%d ack=%p err=%v caller=%s:%d\n", g, &ack, aerr, file, line)
		}
		if aerr != nil {
			unreserveBitmapBits(f, fsOffset, sb, g, true, []int{bit})
			return aerr
		}
		// Commit succeeded; clear reservation for the freed bit.
		unreserveBitmapBits(f, fsOffset, sb, g, true, []int{bit})
		return nil
	}

	// Non-journal path: prepare descriptor bytes under per-group lock,
	// release BGD lock, then write bitmap (we still hold bitmap lock) and
	// finally write the descriptor and superblock.
	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		seed := bitmapCsumSeed(sb, g)
		csum := crc32c(seed, bmap)
		le := binary.LittleEndian
		le.PutUint16(d.raw[24:], uint16(csum&0xFFFF))
		if int(sb.DescSize) >= 64 {
			le.PutUint16(d.raw[56:], uint16(csum>>16))
		}
	}
	d.encode(sb)
	if sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
		le := binary.LittleEndian
		le.PutUint16(d.raw[30:], 0)
		gLE := make([]byte, 4)
		le.PutUint32(gLE, g)
		csum := crc32c(sb.csumSeed(), gLE)
		csum = crc32c(csum, d.raw[:sb.DescSize])
		le.PutUint16(d.raw[30:], uint16(csum))
	}
	rawCopy := make([]byte, len(d.raw))
	copy(rawCopy, d.raw)

	// Install reservation for the cleared bit so other allocators won't
	// pick it before we persist the change.
	reserveBitmapBits(f, sb, g, true, []int{bit}, false)
	// Update in-memory superblock counters and snapshot superblock raw
	atomic.AddUint64(&sb.FreeBlocksCount, 1)
	if ext4LockDebug {
		debugPrintf("DEBUG freeBlock sb.FreeBlocksCount after inc=%d group=%d bit=%d\n", atomic.LoadUint64(&sb.FreeBlocksCount), g, bit)
	}
	bs := int64(sb.BlockSize)
	ssbBlock := uint64(1024 / bs)
	sbUnlock := lockBGDTableBlock(f, ssbBlock)
	sb.encodeRaw()
	sbRawCopy := make([]byte, len(sb.raw))
	copy(sbRawCopy, sb.raw)
	sbUnlock()

	// Snapshot the bitmap while still holding the bitmap lock so the
	// dispatcher writes the intended bytes even after we release the lock.
	bmapCopy := make([]byte, len(bmap))
	copy(bmapCopy, bmap)

	// Release the bitmap lock so other allocators see the reservation,
	// but keep the per-group BGD lock to order enqueues consistently.
	if unlockBitmap != nil {
		unlockBitmap()
		unlockBitmap = nil
	}

	// Persist bitmap, descriptor and superblock via commit dispatcher as a
	// single batch to avoid interleaving with other operations. Keep the
	// BGD lock held until after enqueue to ensure consistent ordering.
	bitmapStart := fsOffset + int64(d.BlockBitmapBlock)*int64(sb.BlockSize)
	tableBlock := sb.bgdTableBlock()
	descOff := int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
	ops := []commitOp{
		{startAbs: bitmapStart, data: bmapCopy},
		{startAbs: fsOffset + descOff, data: rawCopy},
		{startAbs: fsOffset + 1024, data: sbRawCopy},
	}
	ack, seq, err := enqueueCommitWritesUnderLock(f, fsOffset, sb, g, ops)
	if err != nil {
		unreserveBitmapBits(f, fsOffset, sb, g, true, []int{bit})
		unlockBGD()
		return err
	}
	associateAckWithReservation(f, g, true, []int{bit}, ack)
	associateSeqWithReservation(f, g, true, []int{bit}, seq)
	// Release BGD lock after enqueueing to maintain ordering.
	unlockBGD()
	unlockBGD = nil
	// Wait for worker to apply the writes after releasing the lock.
	aerr := <-ack
	if ext4LockDebug {
		_, file, line, _ := runtime.Caller(0)
		debugPrintf("DEBUG ack received freeBlock (non-journal) group=%d ack=%p err=%v caller=%s:%d\n", g, &ack, aerr, file, line)
	}
	if aerr != nil {
		unreserveBitmapBits(f, fsOffset, sb, g, true, []int{bit})
		return aerr
	}
	// Commit succeeded; clear reservation for the freed bit.
	unreserveBitmapBits(f, fsOffset, sb, g, true, []int{bit})
	return nil
}

// findFreeBit finds the index of the first clear bit in bitmap within the
// first maxBits bits.
func findFreeBit(bitmap []byte, maxBits int) (int, bool) {
	for i := 0; i < maxBits; i++ {
		if bitmap[i/8]&(1<<uint(i%8)) == 0 {
			return i, true
		}
	}
	return 0, false
}

// findFreeRun finds a run of n consecutive clear bits.
// Returns the starting index of the run, or false if none found.
func findFreeRun(bitmap []byte, maxBits, n int) ([]int, bool) {
	if ext4LockDebug {
		// Lightweight call-site diagnostic: report invocation and a small
		// sample of bitmap state to help correlate callers when failures
		// occur under concurrency.
		free := 0
		sample := []int{}
		for i := 0; i < maxBits && i < 256; i++ {
			if bitmap[i/8]&(1<<uint(i%8)) == 0 {
				free++
				if len(sample) < 20 {
					sample = append(sample, i)
				}
			}
		}
		_, file, line, _ := runtime.Caller(1)
		debugPrintf("DEBUG findFreeRun called maxBits=%d need=%d prefixFree=%d prefixSample=%v caller=%s:%d\n", maxBits, n, free, sample, file, line)
	}
	if n == 0 {
		return nil, true
	}
	runStart := -1
	runLen := 0
	for i := 0; i < maxBits; i++ {
		if bitmap[i/8]&(1<<uint(i%8)) == 0 {
			if runLen == 0 {
				runStart = i
			}
			runLen++
			if runLen == n {
				result := make([]int, n)
				for j := 0; j < n; j++ {
					result[j] = runStart + j
				}
				return result, true
			}
		} else {
			runLen = 0
			runStart = -1
		}
	}
	// No contiguous run found — collect non-contiguous free bits.
	if runLen < n {
		var bits []int
		for i := 0; i < maxBits && len(bits) < n; i++ {
			if bitmap[i/8]&(1<<uint(i%8)) == 0 {
				bits = append(bits, i)
			}
		}
		if len(bits) == n {
			return bits, true
		}
	}
	return nil, false
}

// countFreeInBitmap returns the number of clear bits in the bitmap (up to
// maxBits) and a small sample of free bit indices useful for diagnostics.
func countFreeInBitmap(bitmap []byte, maxBits int) (int, []int) {
	free := 0
	sample := []int{}
	max := maxBits
	if max > len(bitmap)*8 {
		max = len(bitmap) * 8
	}
	for i := 0; i < max; i++ {
		if bitmap[i/8]&(1<<uint(i%8)) == 0 {
			free++
			if len(sample) < 20 {
				sample = append(sample, i)
			}
		}
	}
	return free, sample
}

// setBit sets bit i in bitmap.
func setBit(bitmap []byte, i int) {
	bitmap[i/8] |= 1 << uint(i%8)
}

// clearBit clears bit i in bitmap.
func clearBit(bitmap []byte, i int) {
	bitmap[i/8] &^= 1 << uint(i%8)
}
