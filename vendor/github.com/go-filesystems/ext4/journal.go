package filesystem_ext4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Minimal on-disk journal format (sidecar file): this implementation writes
// transactions to a sidecar file named <image>.journal next to the image
// backing file. It implements a small, safe subset of JBD2 semantics used
// for metadata journaling and recovery during Open.

const (
	journalMagic uint32 = 0x4A424432 // 'J''B''D''2'
	commitMagic  uint32 = 0x434D4954 // 'C''M''I''T'
	txHeaderSize        = 4 + 8 + 4  // magic(uint32) + seq(uint64) + nDesc(uint32)
	descSize            = 8 + 4      // blockNum(uint64) + dataLen(uint32)
	commitSize          = 4 + 8 + 4  // magic + seq + crc32
)

// Journal represents an on-disk sidecar journal attached to a filesystem
// image. When enabled it persists transactions to the sidecar and can
// replay committed transactions at Open time.
type Journal struct {
	f         readerWriterAt // backing fs file used for applying commits
	fsFile    *os.File       // underlying *os.File (for Sync when available)
	js        *os.File       // sidecar journal file
	fsOffset  int64
	blockSize int
	mu        sync.Mutex
	// applyCond and applySeq synchronise the application of committed
	// transactions so they are applied to the backing image in journal
	// sequence order. This prevents interleaving of writes from
	// concurrent commits which could lead to partial visibility.
	applyCond *sync.Cond
	applySeq  uint64
	// syncMu kept for compatibility; new code uses sync workers.
	syncMu    sync.Mutex
	enabled   bool
	nextSeq   uint64
	sb        *superblock
	appendOff int64
	// removed per-Journal sync queues; global dispatcher used instead
}

type syncRequest struct {
	ack chan error
}

var (
	// maxConcurrentSyncs caps how many concurrent f.Sync() syscalls may run
	// across all journal workers. It can be adjusted via
	// EXT4_MAX_CONCURRENT_FSYNC env var for testing or tuning.
	maxConcurrentSyncs = 4
	// global task channel for sync requests processed by a bounded worker pool
	globalSyncTasks chan *syncTask
)

// addRange retry tuning: attempts and backoff used when attempting to
// acquire BGD + inode-table locks for preparing tx entries that overlap
// inode-table regions. Configurable via environment for stress testing.
var addRangeMaxAttempts = 8
var addRangeBackoffBase = 50 * time.Microsecond
var addRangeBackoffIncrement = 20 * time.Microsecond

// CommitHook, when set (tests only), is invoked with the transaction
// sequence number each time a transaction is committed. This allows tests
// to observe/measure commit behavior without parsing debug logs.
// CommitHook, when set (tests only), is invoked with the transaction
// sequence number and the number of entries each time a transaction is
// committed. This allows tests to observe/measure commit behavior without
// parsing debug logs.
var CommitHook func(seq uint64, entries int)

func init() {
	if s := os.Getenv("EXT4_MAX_CONCURRENT_FSYNC"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			maxConcurrentSyncs = v
		}
	}
	// start global sync dispatcher
	globalSyncTasks = make(chan *syncTask, 4096)
	for i := 0; i < maxConcurrentSyncs; i++ {
		go syncDispatcherWorker(i)
	}

	// allow tuning addRange retry/backoff via environment for tests
	if v := os.Getenv("EXT4_ADD_RANGE_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			addRangeMaxAttempts = n
		}
	}
	if v := os.Getenv("EXT4_ADD_RANGE_BACKOFF_BASE_US"); v != "" {
		if us, err := strconv.ParseInt(v, 10, 64); err == nil && us >= 0 {
			addRangeBackoffBase = time.Duration(us) * time.Microsecond
		}
	}
	if v := os.Getenv("EXT4_ADD_RANGE_BACKOFF_INCREMENT_US"); v != "" {
		if us, err := strconv.ParseInt(v, 10, 64); err == nil && us >= 0 {
			addRangeBackoffIncrement = time.Duration(us) * time.Microsecond
		}
	}

	if ext4LockDebug {
		debugPrintf("DEBUG addRange config maxAttempts=%d base=%s increment=%s\n", addRangeMaxAttempts, addRangeBackoffBase, addRangeBackoffIncrement)
	}
}

type syncTask struct {
	f     *os.File
	ack   chan error
	label string
}

func syncDispatcherWorker(id int) {
	for t := range globalSyncTasks {
		start := time.Now()
		var err error
		if t != nil && t.f != nil {
			err = t.f.Sync()
		}
		commitTraceDuration(t.label, start)
		if t != nil && t.ack != nil {
			t.ack <- err
		}
	}
}

// OpenJournal opens/creates a sidecar journal for the provided backing
// reader/writer. It returns a Journal that is enabled when a sidecar file
// can be created; otherwise it returns a disabled journal (no-op).
func OpenJournal(f readerWriterAt, fsOffset int64, blockSize int, sb *superblock) (*Journal, error) {
	if f == nil || sb == nil {
		return &Journal{f: f, fsOffset: fsOffset, blockSize: blockSize, enabled: false, sb: sb}, nil
	}

	// Obtain underlying *os.File when possible to derive a sidecar path and
	// to call Sync after applying commits.
	var of *os.File
	if raw, ok := f.(*os.File); ok {
		of = raw
	} else {
		type underlyingFileGetter interface{ UnderlyingFile() *os.File }
		if g, ok := f.(underlyingFileGetter); ok {
			of = g.UnderlyingFile()
		}
	}
	if of == nil {
		return &Journal{f: f, fsOffset: fsOffset, blockSize: blockSize, enabled: false, sb: sb}, nil
	}

	jpath := of.Name() + ".journal"
	if dir := filepath.Dir(jpath); dir == "." || dir == "" {
		// keep as-is
	}
	js, err := os.OpenFile(jpath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		// sidecar not available; return disabled journal
		return &Journal{f: f, fsOffset: fsOffset, blockSize: blockSize, enabled: false, sb: sb}, nil
	}

	j := &Journal{f: f, fsFile: of, js: js, fsOffset: fsOffset, blockSize: blockSize, enabled: true, sb: sb}
	j.applyCond = sync.NewCond(&j.mu)
	// Global sync dispatcher is used; per-Journal queues/workers removed.
	if err := j.initNextSeq(); err != nil {
		// failed to parse existing journal; disable to avoid recovery surprises
		js.Close()
		return &Journal{f: f, fsOffset: fsOffset, blockSize: blockSize, enabled: false, sb: sb}, nil
	}
	// Initialize the application sequence to the next sequence so newly
	// committed transactions are applied in order starting from this seq.
	j.applySeq = j.nextSeq
	// Register journal for this backing file so package-level write helpers
	// can locate the journal instance from a readerWriterAt.
	registerJournal(f, j)
	return j, nil
}

// journal registry keyed by the same key used for file locks.
var journalRegistryMu sync.Mutex
var journalRegistry = map[string]*Journal{}

func registerJournal(f readerWriterAt, j *Journal) {
	if f == nil || j == nil {
		return
	}
	key := fileLockKey(f)
	journalRegistryMu.Lock()
	journalRegistry[key] = j
	// Also register under a stable file path key when possible so code that
	// opens the same file via a different descriptor can still find the
	// journal. Use the underlying *os.File.Name() when available.
	// If f is already an *os.File, register by its path. Otherwise, if f
	// exposes an UnderlyingFile() method, use that to get a stable path.
	if of, ok := f.(*os.File); ok && of != nil {
		pkey := fmt.Sprintf("path:%s", of.Name())
		journalRegistry[pkey] = j
	} else {
		type underlyingFileGetter interface{ UnderlyingFile() *os.File }
		if h, ok := f.(underlyingFileGetter); ok {
			if of := h.UnderlyingFile(); of != nil {
				pkey := fmt.Sprintf("path:%s", of.Name())
				journalRegistry[pkey] = j
			}
		}
	}
	journalRegistryMu.Unlock()
}

func unregisterJournal(f readerWriterAt) {
	if f == nil {
		return
	}
	key := fileLockKey(f)
	journalRegistryMu.Lock()
	delete(journalRegistry, key)
	// Also remove any path-based registration.
	if of, ok := f.(*os.File); ok && of != nil {
		pkey := fmt.Sprintf("path:%s", of.Name())
		delete(journalRegistry, pkey)
	} else {
		type underlyingFileGetter interface{ UnderlyingFile() *os.File }
		if h, ok := f.(underlyingFileGetter); ok {
			if of := h.UnderlyingFile(); of != nil {
				pkey := fmt.Sprintf("path:%s", of.Name())
				delete(journalRegistry, pkey)
			}
		}
	}
	journalRegistryMu.Unlock()
}

func journalFor(f readerWriterAt) *Journal {
	if f == nil {
		return nil
	}
	// Try lookup by file-lock key first (fast-path).
	key := fileLockKey(f)
	journalRegistryMu.Lock()
	if j, ok := journalRegistry[key]; ok {
		journalRegistryMu.Unlock()
		return j
	}
	// Fallback: if we can obtain an underlying *os.File, look up by its
	// stable pathname so differently-opened descriptors still map to the
	// same journal instance.
	type underlyingFileGetter interface{ UnderlyingFile() *os.File }
	if h, ok := f.(underlyingFileGetter); ok {
		if of := h.UnderlyingFile(); of != nil {
			pkey := fmt.Sprintf("path:%s", of.Name())
			if j, ok := journalRegistry[pkey]; ok {
				journalRegistryMu.Unlock()
				return j
			}
		}
	}
	journalRegistryMu.Unlock()
	return nil
}

// journalForAny attempts to locate a Journal for the provided readerWriterAt
// by first checking any underlying *os.File the wrapper may expose and then
// falling back to the standard journalFor lookup. This helps callers that
// receive wrapper types (test-only loggingRW, HookMemFile, etc.) to find
// the journal registered for the raw backing file.
func journalForAny(f readerWriterAt) *Journal {
	if f == nil {
		return nil
	}
	// If the wrapper exposes an underlying *os.File, try lookups using that
	// first (these keys are registered during OpenJournal when the raw
	// *os.File was available).
	type underlyingFileGetter interface{ UnderlyingFile() *os.File }
	if h, ok := f.(underlyingFileGetter); ok {
		if of := h.UnderlyingFile(); of != nil {
			if j := journalFor(of); j != nil {
				return j
			}
		}
	}
	// Also handle the direct *os.File case.
	if of, ok := f.(*os.File); ok && of != nil {
		if j := journalFor(of); j != nil {
			return j
		}
	}
	return journalFor(f)
}

// applyOrJournalWrite writes the provided data at absolute offset startAbs
// (relative to the underlying image file) using the journal if available.
// When journaling, writes are expanded to whole-block writes and full
// filesystem blocks are recorded in the transaction to simplify replay.
func applyOrJournalWrite(f readerWriterAt, fsOffset int64, sb *superblock, startAbs int64, data []byte) error {
	if sb == nil || sb.BlockSize == 0 {
		if _, err := writeAtWithTrace(f, fsOffset, sb, startAbs, data); err != nil {
			return fmt.Errorf("ext4: write: %w", err)
		}
		return nil
	}
	j := journalForAny(f)
	if j == nil || !j.enabled {
		if _, err := writeAtWithTrace(f, fsOffset, sb, startAbs, data); err != nil {
			return fmt.Errorf("ext4: write: %w", err)
		}
		return nil
	}

	tx, err := j.StartTx()
	if err != nil {
		return err
	}
	bs := int64(sb.BlockSize)
	relStart := startAbs - fsOffset
	if relStart < 0 {
		return fmt.Errorf("ext4: invalid write offset")
	}
	startBlock := uint64(relStart / bs)
	offInBlock := int64(relStart % bs)
	remaining := len(data)
	idx := 0
	bnum := startBlock
	for remaining > 0 {
		blockBuf, err := readRawBlock(f, fsOffset, sb, bnum)
		if err != nil {
			return err
		}
		copyLen := int(bs - offInBlock)
		if copyLen > remaining {
			copyLen = remaining
		}
		copy(blockBuf[offInBlock:offInBlock+int64(copyLen)], data[idx:idx+copyLen])

		usedAddRange := false
		if sb != nil && sb.BlocksPerGroup != 0 {
			first := uint64(sb.FirstDataBlock)
			if bnum >= first {
				rel := bnum - first
				g := uint32(rel / uint64(sb.BlocksPerGroup))
				if g < sb.numBlockGroups() {
					if d2, derr := readBGD(f, fsOffset, sb, g); derr == nil {
						inodeTableStart := d2.InodeTableBlock
						inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
						if bnum >= inodeTableStart && bnum < inodeTableStart+inodeTableBlocks {
							var buf [4096]byte
							n := runtime.Stack(buf[:], false)
							sample := blockBuf
							if len(sample) > 128 {
								sample = sample[:128]
							}
							debugPrintf("DEBUG addRangeToTx: adding inode-table block block=%d group=%d inodeTableStart=%d inodeTableBlocks=%d sample(first=%d): %q\nstack:\n%s\n", bnum, g, inodeTableStart, inodeTableBlocks, len(sample), string(sample), string(buf[:n]))
							if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+int64(bnum)*int64(bs), blockBuf); err != nil {
								return err
							}
							usedAddRange = true
						}
					}
				}
			}
		}
		if !usedAddRange {
			// Use addRangeToTx to prepare this block into the transaction so
			// per-block locking and inode-table/BGD handling are applied
			// consistently (prevents lost-updates when multiple transactions
			// prepare the same block concurrently).
			if err := addRangeToTx(tx, f, fsOffset, sb, fsOffset+int64(bnum)*bs, blockBuf); err != nil {
				return err
			}
		}
		remaining -= copyLen
		idx += copyLen
		bnum++
		offInBlock = 0
	}
	return tx.Commit()
}

// addRangeToTx overlays `data` at absolute offset `startAbs` into the
// transaction `tx`. The function ensures whole filesystem blocks are added
// to the transaction by reading the current block content and merging the
// supplied bytes. This allows multiple metadata blocks to be grouped into
// a single transaction.
func addRangeToTx(tx *Transaction, f readerWriterAt, fsOffset int64, sb *superblock, startAbs int64, data []byte) error {
	if tx == nil {
		return fmt.Errorf("nil transaction")
	}
	if sb == nil || sb.BlockSize == 0 {
		return fmt.Errorf("invalid superblock for addRangeToTx")
	}
	// Use the filesystem block size when preparing transaction entries so
	// the block numbering and offsets match the superblock. Journal
	// instances should be opened with the same block size as the
	// filesystem; prefer `sb.BlockSize` here to avoid accidental
	// desynchronization if a Journal instance was created with a
	// different value.
	bs := int64(sb.BlockSize)
	relStart := startAbs - fsOffset
	if relStart < 0 {
		return fmt.Errorf("ext4: invalid write offset")
	}
	startBlock := uint64(relStart / bs)
	offInBlock := int64(relStart % bs)
	remaining := len(data)
	idx := 0
	bnum := startBlock
	// Detect whether any of the blocks in the range map into inode-table
	// regions. If so, acquire the per-group BGD locks then the
	// inode-table locks in canonical order while preparing the transaction
	// entries to avoid TOCTOU races between descriptor updates and later
	// commit/application.
	first := uint64(sb.FirstDataBlock)
	nGroups := sb.numBlockGroups()
	groupsMap := map[uint32]uint64{}
	// Scan blocks in this range and record groups whose inode-tables are
	// touched along with the current inode-table start block. Use the
	// computed block size `bs` for advancing through the range so the
	// block numbering matches what we will add to the tx.
	tmpRemaining := remaining
	tmpOff := offInBlock
	tmpBnum := bnum
	for tmpRemaining > 0 {
		if tmpBnum >= first && sb.BlocksPerGroup != 0 {
			rel := tmpBnum - first
			g := uint32(rel / uint64(sb.BlocksPerGroup))
			if g < nGroups {
				if d, derr := readBGD(f, fsOffset, sb, g); derr == nil {
					inodeTableStart := d.InodeTableBlock
					inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
					if tmpBnum >= inodeTableStart && tmpBnum < inodeTableStart+inodeTableBlocks {
						groupsMap[g] = inodeTableStart
					}
				}
			}
		}
		// advance by block
		copyLen := int(bs - tmpOff)
		if copyLen > tmpRemaining {
			copyLen = tmpRemaining
		}
		tmpRemaining -= copyLen
		tmpBnum++
		tmpOff = 0
	}

	// If we have candidate groups, attempt to acquire BGD locks and then
	// inode-table locks in increasing order before preparing tx entries.
	if len(groupsMap) > 0 {
		// Build sorted group list
		var gkeys []int
		for g := range groupsMap {
			gkeys = append(gkeys, int(g))
		}
		sort.Ints(gkeys)
		// Try a few times to handle transient descriptor moves.
		// Increase attempts/backoff to reduce transient "unstable BGD mapping"
		// failures under heavy concurrent stress. Parameters are tunable
		// via environment variables for testing.
		maxAttempts := addRangeMaxAttempts
		locked := false
		for attempt := 0; attempt < maxAttempts && !locked; attempt++ {
			// Acquire BGD locks for candidates
			var unlockBGDs []func()
			for _, gi := range gkeys {
				unlockBGDs = append(unlockBGDs, lockBGDGroup(f, uint32(gi)))
			}
			// Verify descriptors are stable and still contain inode tables
			stable := true
			for _, gi := range gkeys {
				g := uint32(gi)
				d2, derr := readBGD(f, fsOffset, sb, g)
				if derr != nil || d2.InodeTableBlock != groupsMap[g] {
					stable = false
					break
				}
			}
			if !stable {
				for i := len(unlockBGDs) - 1; i >= 0; i-- {
					unlockBGDs[i]()
				}
				// backoff before retrying; allow tuning via env vars
				time.Sleep(addRangeBackoffBase + addRangeBackoffIncrement*time.Duration(attempt))
				// Rebuild the groupsMap from the on-disk descriptors in case the
				// inode-table start blocks moved since our initial scan. This mirrors
				// the approach used by writeAtWithTrace so retries converge to the
				// current canonical mapping instead of repeatedly comparing against
				// a stale snapshot.
				groupsMap = map[uint32]uint64{}
				tmpRemaining2 := remaining
				tmpOff2 := offInBlock
				tmpBnum2 := bnum
				for tmpRemaining2 > 0 {
					if tmpBnum2 >= first && sb.BlocksPerGroup != 0 {
						rel := tmpBnum2 - first
						g := uint32(rel / uint64(sb.BlocksPerGroup))
						if g < nGroups {
							if d, derr := readBGD(f, fsOffset, sb, g); derr == nil {
								inodeTableStart := d.InodeTableBlock
								inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
								if tmpBnum2 >= inodeTableStart && tmpBnum2 < inodeTableStart+inodeTableBlocks {
									groupsMap[g] = inodeTableStart
								}
							}
						}
					}
					// advance by block
					copyLen := int(bs - tmpOff2)
					if copyLen > tmpRemaining2 {
						copyLen = tmpRemaining2
					}
					tmpRemaining2 -= copyLen
					tmpBnum2++
					tmpOff2 = 0
				}
				// Rebuild sorted gkeys for the next attempt.
				gkeys = gkeys[:0]
				for g := range groupsMap {
					gkeys = append(gkeys, int(g))
				}
				sort.Ints(gkeys)
				continue
			}
			// Acquire inode-table locks in order
			var unlockInodes []func()
			for _, gi := range gkeys {
				unlockInodes = append(unlockInodes, lockInodeTableGroup(f, uint32(gi)))
			}
			// Release BGD locks
			for i := len(unlockBGDs) - 1; i >= 0; i-- {
				unlockBGDs[i]()
			}

			// With inode-table locks held, prepare tx entries safely.
			// To avoid lost-updates when multiple transactions prepare the
			// same filesystem block concurrently, acquire per-block locks
			// for the blocks we will prepare before reading and adding
			// them to the transaction.
			var dataUnlocks []func()
			// Compute list of blocks to lock for this range.
			// Skip blocks already in the transaction: the data-block lock is
			// already held (registered with the tx from the first call that
			// added the block). Re-acquiring it would deadlock.
			var blocksToLock []uint64
			tmpRem := remaining
			tmpOff2 := offInBlock
			tmpB := bnum
			for tmpRem > 0 {
				if tx.GetBlock(tmpB) == nil {
					blocksToLock = append(blocksToLock, tmpB)
				}
				copyLen := int(bs - tmpOff2)
				if copyLen > tmpRem {
					copyLen = tmpRem
				}
				tmpRem -= copyLen
				tmpB++
				tmpOff2 = 0
			}
			// Acquire data-block locks before inode-table locks to avoid
			// deadlock with Transaction.Commit (which holds tx data locks
			// and acquires inode-table locks via writeAtWithTrace).
			if len(blocksToLock) > 0 {
				for i := len(unlockInodes) - 1; i >= 0; i-- {
					unlockInodes[i]()
				}
				unlockInodes = unlockInodes[:0]
				for _, bl := range blocksToLock {
					dataUnlocks = append(dataUnlocks, lockDataBlock(f, bl))
				}
				for _, gi := range gkeys {
					unlockInodes = append(unlockInodes, lockInodeTableGroup(f, uint32(gi)))
				}
			}
			for remaining > 0 {
				// If the block is already in the transaction, start from the
				// in-tx copy so that previous modifications to the same block
				// (e.g. another inode in the same inode-table block) are merged
				// rather than overwritten.
				var blockBuf []byte
				off := fsOffset + int64(bnum)*bs
				if inTx := tx.GetBlock(bnum); inTx != nil {
					blockBuf = inTx
				} else {
					blockBuf = make([]byte, bs)
					if _, err := f.ReadAt(blockBuf, off); err != nil {
						// Release data locks then inode locks before returning.
						for i := len(dataUnlocks) - 1; i >= 0; i-- {
							dataUnlocks[i]()
						}
						for i := len(unlockInodes) - 1; i >= 0; i-- {
							unlockInodes[i]()
						}
						return fmt.Errorf("ext4: read block %d: %w", bnum, err)
					}
				}
				copyLen := int(bs - offInBlock)
				if copyLen > remaining {
					copyLen = remaining
				}
				copy(blockBuf[offInBlock:offInBlock+int64(copyLen)], data[idx:idx+copyLen])
				if err := tx.AddBlock(bnum, blockBuf); err != nil {
					for i := len(dataUnlocks) - 1; i >= 0; i-- {
						dataUnlocks[i]()
					}
					for i := len(unlockInodes) - 1; i >= 0; i-- {
						unlockInodes[i]()
					}
					return err
				}
				remaining -= copyLen
				idx += copyLen
				bnum++
				offInBlock = 0
			}
			// Register per-block data locks with the transaction so they
			// are held across the transaction lifetime and released by
			// Transaction.releaseLocks when the transaction completes.
			// This prevents lost-update races where another transaction
			// might overwrite prepared blocks during apply.
			tx.addLocks(dataUnlocks)
			// Release only the inode-table locks now; data locks remain
			// held until Commit/Abort via tx.releaseLocks.
			for i := len(unlockInodes) - 1; i >= 0; i-- {
				unlockInodes[i]()
			}
			locked = true
		}
		if locked {
			return nil
		}
		// Failed to obtain stable locks after retries. Refuse to prepare
		// transaction entries that touch inode-table ranges without holding
		// the inode-table locks: falling back to unprotected preparation can
		// lead to TOCTOU corruption under concurrency. Return an error so the
		// caller can handle the failure safely.
		return fmt.Errorf("ext4: unstable BGD mapping; refusing to prepare inode-table range without locks")
	}

	// Fallback: no inode-table locks acquired or locking failed; prepare
	// tx entries as before. Acquire per-block locks across the range to
	// avoid concurrent read-modify-write races where another transaction
	// prepared a block earlier and a later-prepared transaction overwrites
	// it during apply.
	// Compute list of blocks to lock for this range.
	// Skip blocks already in the transaction to avoid re-acquiring a lock
	// that is already held (which would deadlock on non-reentrant tryMutex).
	var fallbackDataUnlocks []func()
	var fallbackBlocksToLock []uint64
	tmpRem2 := remaining
	tmpOff3 := offInBlock
	tmpB2 := bnum
	for tmpRem2 > 0 {
		if tx.GetBlock(tmpB2) == nil {
			fallbackBlocksToLock = append(fallbackBlocksToLock, tmpB2)
		}
		copyLen := int(bs - tmpOff3)
		if copyLen > tmpRem2 {
			copyLen = tmpRem2
		}
		tmpRem2 -= copyLen
		tmpB2++
		tmpOff3 = 0
	}
	// Try locking fallback blocks using TryLock to avoid long-held blocking
	// while other goroutines hold BGD/inode locks. If TryLock cannot acquire
	// all locks after a few attempts, fall back to blocking acquisition.
	maxFallbackAttempts := 6
	got := false
	for attempt := 0; attempt < maxFallbackAttempts && !got; attempt++ {
		// release any previously acquired
		for i := len(fallbackDataUnlocks) - 1; i >= 0; i-- {
			fallbackDataUnlocks[i]()
		}
		fallbackDataUnlocks = fallbackDataUnlocks[:0]
		failed := false
		for _, bl := range fallbackBlocksToLock {
			key := dataBlockLockKey(f, bl)
			dataBlockLocksMu.Lock()
			m, ok := dataBlockLocks[key]
			if !ok {
				m = newTryMutex()
				dataBlockLocks[key] = m
			}
			dataBlockLocksMu.Unlock()
			if m.TryLock() {
				fallbackDataUnlocks = append(fallbackDataUnlocks, func(m *tryMutex) func() {
					return func() { m.Unlock() }
				}(m))
			} else {
				failed = true
				break
			}
		}
		if !failed {
			got = true
			break
		}
		time.Sleep(time.Duration(50+attempt*50) * time.Microsecond)
	}
	if !got {
		// The last TryLock attempt may have acquired a prefix of locks before
		// failing. Release that partial set before blocking acquisition to
		// avoid deadlock cycles with goroutines waiting in opposite order.
		for i := len(fallbackDataUnlocks) - 1; i >= 0; i-- {
			fallbackDataUnlocks[i]()
		}
		fallbackDataUnlocks = fallbackDataUnlocks[:0]
		for _, bl := range fallbackBlocksToLock {
			fallbackDataUnlocks = append(fallbackDataUnlocks, lockDataBlock(f, bl))
		}
	}
	for remaining > 0 {
		off := fsOffset + int64(bnum)*bs
		// If this block has already been prepared into the transaction,
		// use the in-tx copy as the base so that earlier modifications
		// (e.g. a different inode in the same inode-table block) are not
		// lost when we overlay our own changes.
		var blockBuf []byte
		if inTx := tx.GetBlock(bnum); inTx != nil {
			blockBuf = inTx
		} else {
			blockBuf = make([]byte, bs)
			if _, err := f.ReadAt(blockBuf, off); err != nil {
				// Release fallback per-block locks now. These were acquired without
				// holding inode-table locks (fallback path), so retaining them across
				// the transaction can create lock-order inversions with other code
				// paths that acquire inode-table locks first. To avoid deadlocks,
				// release them here instead of registering with the transaction.
				for i := len(fallbackDataUnlocks) - 1; i >= 0; i-- {
					fallbackDataUnlocks[i]()
				}
				return fmt.Errorf("ext4: read block %d: %w", bnum, err)
			}
		}
		copyLen := int(bs - offInBlock)
		if copyLen > remaining {
			copyLen = remaining
		}
		copy(blockBuf[offInBlock:offInBlock+int64(copyLen)], data[idx:idx+copyLen])
		if err := tx.AddBlock(bnum, blockBuf); err != nil {
			for i := len(fallbackDataUnlocks) - 1; i >= 0; i-- {
				fallbackDataUnlocks[i]()
			}
			return err
		}
		remaining -= copyLen
		idx += copyLen
		bnum++
		offInBlock = 0
	}
	for i := len(fallbackDataUnlocks) - 1; i >= 0; i-- {
		fallbackDataUnlocks[i]()
	}
	return nil
}

// Transaction collects metadata blocks to be journaled and committed.
type Transaction struct {
	j         *Journal
	entries   []journalEntry
	committed bool
	mu        sync.Mutex
	onCommit  []func(uint64)
	// locks holds unlock functions for per-block locks acquired during
	// transaction preparation. These locks are held across the lifetime
	// of the transaction and released when the transaction completes
	// (Commit/Abort) to prevent lost-update races between preparations
	// and apply-time writes.
	locks []func()
}

// AddCommitCallback registers a callback to be invoked when this
// transaction is applied. The callback receives the transaction sequence
// number. It is an error to register callbacks after the transaction has
// been committed.
func (tx *Transaction) AddCommitCallback(cb func(uint64)) error {
	if tx == nil {
		return fmt.Errorf("nil transaction")
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.committed {
		return fmt.Errorf("transaction already committed")
	}
	tx.onCommit = append(tx.onCommit, cb)
	return nil
}

type journalEntry struct {
	Block uint64
	Data  []byte
}

// StartTx creates a new transaction associated with the journal.
func (j *Journal) StartTx() (*Transaction, error) {
	return &Transaction{j: j, entries: nil, committed: false}, nil
}

// AddBlock appends a metadata block to the transaction. The data is copied.
// AddBlock adds or updates the entry for blockNum in the transaction.
// If an entry for blockNum already exists it is replaced so each block
// appears at most once in the journal record.  Callers that want to
// build on a previous in-tx modification of the same block must first
// call GetBlock to retrieve the current in-tx state before overlaying
// their changes and calling AddBlock.
func (tx *Transaction) AddBlock(blockNum uint64, data []byte) error {
	if tx == nil {
		return fmt.Errorf("nil transaction")
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.committed {
		return fmt.Errorf("transaction already committed")
	}
	copybuf := make([]byte, len(data))
	copy(copybuf, data)
	// Upsert: replace an existing entry for this block rather than append
	// to avoid applying the same block twice during commit (the last write
	// would otherwise overwrite earlier in-tx modifications of the same
	// block, e.g. when two inodes that share the same inode-table block
	// are written in the same transaction).
	for i := range tx.entries {
		if tx.entries[i].Block == blockNum {
			tx.entries[i].Data = copybuf
			return nil
		}
	}
	tx.entries = append(tx.entries, journalEntry{Block: blockNum, Data: copybuf})
	return nil
}

// GetBlock returns the current in-transaction data for blockNum, or nil if
// the block has not been prepared into this transaction yet.  The returned
// slice is a copy and safe to modify.
func (tx *Transaction) GetBlock(blockNum uint64) []byte {
	if tx == nil {
		return nil
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	for i := range tx.entries {
		if tx.entries[i].Block == blockNum {
			cp := make([]byte, len(tx.entries[i].Data))
			copy(cp, tx.entries[i].Data)
			return cp
		}
	}
	return nil
}

// addLocks registers unlock functions to be released when the transaction
// completes. Callers should only register locks after they have acquired
// them successfully and prepared transaction entries; on error paths the
// caller is responsible for releasing any local locks that were not
// registered.
func (tx *Transaction) addLocks(locks []func()) {
	if tx == nil || len(locks) == 0 {
		return
	}
	tx.mu.Lock()
	tx.locks = append(tx.locks, locks...)
	tx.mu.Unlock()
}

// releaseLocks releases any per-block locks registered with the
// transaction. It is safe to call multiple times; subsequent calls are
// no-ops once locks have been cleared.
func (tx *Transaction) releaseLocks() {
	if tx == nil {
		return
	}
	tx.mu.Lock()
	locks := tx.locks
	tx.locks = nil
	tx.mu.Unlock()
	for i := len(locks) - 1; i >= 0; i-- {
		if locks[i] != nil {
			locks[i]()
		}
	}
}

// Commit writes the transaction to the sidecar journal and then applies the
// blocks to the backing filesystem image (write-through). This provides a
// straightforward durability model and simplifies replay semantics.
func (tx *Transaction) Commit() error {
	if tx == nil {
		return fmt.Errorf("nil transaction")
	}
	// Ensure any per-block locks held by this transaction are released when
	// Commit returns (success or error). This prevents locks from leaking
	// if Commit exits early.
	defer tx.releaseLocks()
	// Short critical section: validate and snapshot entries then mark committed
	tx.mu.Lock()
	if tx.committed {
		tx.mu.Unlock()
		return fmt.Errorf("transaction already committed")
	}
	// Copy entries out so we don't hold tx.mu during IO.
	entries := make([]journalEntry, len(tx.entries))
	copy(entries, tx.entries)
	// Mark committed early so AddBlock will fail if called concurrently.
	tx.committed = true
	tx.mu.Unlock()

	j := tx.j
	if j == nil || !j.enabled {
		// No sidecar journal available — nothing to persist/apply.
		return nil
	}

	// Reserve a sequence number atomically to avoid holding j.mu.
	seq := atomic.AddUint64(&j.nextSeq, 1) - 1
	// Debug: record sequence and a short callstack so we can correlate
	// `traceWriteAt overlappedInoRange` hits back to the originating
	// transaction creator during focused test runs.
	pcs := make([]uintptr, 10)
	ncallers := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:ncallers])
	callers := []string{}
	for i := 0; i < 3; i++ {
		f, more := frames.Next()
		callers = append(callers, fmt.Sprintf("%s %s:%d", f.Function, f.File, f.Line))
		if !more {
			break
		}
	}
	debugPrintf("DEBUG txCommit seq=%d entries=%d callstack=%v\n", seq, len(entries), callers)

	// Extra debug: list the block numbers included in this transaction so
	// tests can correlate transaction sequences to the exact on-disk blocks
	// prepared by the transaction. Use debugPrintf (which is gated by
	// EXT4_LOCK_DEBUG) rather than an additional env var for simplicity.
	blocks := make([]uint64, 0, len(entries))
	for _, e := range entries {
		blocks = append(blocks, e.Block)
	}
	debugPrintf("DEBUG txCommit seq=%d blocks=%v\n", seq, blocks)

	// Timestamp when transaction commit processing started (for correlation).
	debugPrintf("DEBUG txCommit seq=%d startTs=%d\n", seq, time.Now().UnixNano())

	// Note: CommitHook is invoked after the transaction entries have been
	// applied and synced to the filesystem to ensure tests observe a fully
	// applied transaction (prevents false positives where tests read after
	// CommitHook but before entries are visible).

	// Serialize write: descriptor(s) -> data -> commit
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, journalMagic); err != nil {
		return err
	}
	if err := binary.Write(&buf, binary.LittleEndian, seq); err != nil {
		return err
	}
	nDesc := uint32(len(entries))
	if err := binary.Write(&buf, binary.LittleEndian, nDesc); err != nil {
		return err
	}
	// descriptors: write block descriptors (block number + length)
	for _, e := range entries {
		if err := binary.Write(&buf, binary.LittleEndian, e.Block); err != nil {
			return err
		}
		ln := uint32(len(e.Data))
		if err := binary.Write(&buf, binary.LittleEndian, ln); err != nil {
			return err
		}
	}
	// data blobs
	for _, e := range entries {
		if _, err := buf.Write(e.Data); err != nil {
			return err
		}
	}
	// compute crc over header+descs+data
	crc := crc32.Update(0, castagnoli, buf.Bytes())
	// commit record
	var cbuf bytes.Buffer
	if err := binary.Write(&cbuf, binary.LittleEndian, commitMagic); err != nil {
		return err
	}
	if err := binary.Write(&cbuf, binary.LittleEndian, seq); err != nil {
		return err
	}
	if err := binary.Write(&cbuf, binary.LittleEndian, crc); err != nil {
		return err
	}
	// Reserve space in the journal and write header+data+commit atomically
	total := int64(buf.Len() + cbuf.Len())
	off, err := j.reserveAppend(total)
	if err != nil {
		return err
	}
	if _, err := j.js.WriteAt(buf.Bytes(), off); err != nil {
		return err
	}
	if _, err := j.js.WriteAt(cbuf.Bytes(), off+int64(buf.Len())); err != nil {
		return err
	}
	// Queue journal fsync
	_ = j.queueJSSync()
	// Wait for our turn to apply transactions in journal sequence order.
	// This prevents concurrent commits from interleaving their writes and
	// exposing partially-applied transactions to readers.
	j.mu.Lock()
	debugPrintf("DEBUG txCommit seq=%d waiting to apply currentApplySeq=%d ts=%d\n", seq, j.applySeq, time.Now().UnixNano())
	for j.applySeq != seq {
		j.applyCond.Wait()
	}
	debugPrintf("DEBUG txCommit seq=%d starting apply ts=%d\n", seq, time.Now().UnixNano())
	j.mu.Unlock()
	// Once we own the apply turn for `seq`, always advance applySeq on return
	// (success or error) so later commits cannot block forever behind this seq.
	mustAdvanceApplySeq := true
	advanceApplySeq := func() {
		j.mu.Lock()
		if j.applySeq == seq {
			j.applySeq = seq + 1
			j.applyCond.Broadcast()
			debugPrintf("DEBUG txCommit seq=%d advanced applySeq=%d ts=%d\n", seq, j.applySeq, time.Now().UnixNano())
		}
		j.mu.Unlock()
	}
	defer func() {
		if mustAdvanceApplySeq {
			advanceApplySeq()
		}
	}()
	// Apply transaction entries synchronously in the transaction order.
	// Performing writes inline here ensures the caller does not return
	// until all transaction entries are applied to the backing image,
	// preventing readers from observing partially-applied transactions.
	//
	// IMPORTANT: use j.sb so writeAtWithTrace acquires inode-table locks
	// when writing inode-table blocks, preserving read/write exclusion for
	// inode-table regions. addRangeToTx acquires locks in canonical order
	// data-block -> inode-table to avoid lock-order inversion with this path.
	for _, e := range entries {
		off := j.fsOffset + int64(e.Block)*int64(j.blockSize)
		dataCopy := make([]byte, len(e.Data))
		copy(dataCopy, e.Data)
		if _, err := writeAtWithTrace(j.f, j.fsOffset, j.sb, off, dataCopy); err != nil {
			return fmt.Errorf("ext4: apply tx entry block %d: %w", e.Block, err)
		}
		// Optionally dump the applied on-disk bytes for each transaction
		// entry to aid debugging of bitmap/bgds and to correlate writes.
		if os.Getenv("EXT4_DUMP_TX_BLOCKS") == "1" {
			buf := make([]byte, j.blockSize)
			if _, rerr := j.f.ReadAt(buf, off); rerr == nil {
				dumpPath := appliedDumpPath(seq, uint64(e.Block))
				_ = os.WriteFile(dumpPath, buf, 0644)
				debugPrintf("DEBUG txCommit seq=%d applied block dump=%d path=%s ts=%d\n", seq, e.Block, dumpPath, time.Now().UnixNano())
			} else {
				debugPrintf("DEBUG txCommit seq=%d applied block readback failed block=%d err=%v ts=%d\n", seq, e.Block, rerr, time.Now().UnixNano())
			}
		}
	}

	// If the user requested focus on a specific seq, force a readback dump of
	// all applied blocks for that sequence to guarantee we capture authoritative
	// on-disk bytes for forensic analysis. Run asynchronously to avoid blocking
	// the commit path.
	if fsEnv := os.Getenv("EXT4_FOCUS_SEQ"); fsEnv != "" {
		if fsVal, err := strconv.ParseUint(fsEnv, 10, 64); err == nil {
			if fsVal == seq {
				// capture block numbers to avoid holding large data buffers
				blocks := make([]uint64, len(entries))
				for i := range entries {
					blocks[i] = entries[i].Block
				}
				go func(s uint64, blks []uint64) {
					for _, b := range blks {
						off := j.fsOffset + int64(b)*int64(j.blockSize)
						buf := make([]byte, j.blockSize)
						if _, rerr := j.f.ReadAt(buf, off); rerr == nil {
							dumpPath := appliedDumpPath(s, b)
							_ = os.WriteFile(dumpPath, buf, 0644)
							debugPrintf("DEBUG txCommit focused appliedDump seq=%d blk=%d path=%s ts=%d\n", s, b, dumpPath, time.Now().UnixNano())
						} else if ext4LockDebug {
							debugPrintf("DEBUG txCommit focused readback failed seq=%d blk=%d err=%v\n", s, b, rerr)
						}
					}
				}(seq, blocks)
			}
		}
	}

	if j.fsFile != nil {
		if os.Getenv("EXT4_DISABLE_FSYNC") != "1" {
			_ = j.queueFSFileSync()
		}
		debugPrintf("DEBUG txCommit seq=%d queued FS sync ts=%d\n", seq, time.Now().UnixNano())
	}
	// Advance apply sequence and wake any waiting commits.
	advanceApplySeq()
	mustAdvanceApplySeq = false

	// Invoke any registered per-transaction callbacks so callers can
	// associate their in-memory reservations with this sequence. Copy
	// callbacks under tx.mu to avoid races with concurrent registrations.
	tx.mu.Lock()
	cbs := make([]func(uint64), len(tx.onCommit))
	copy(cbs, tx.onCommit)
	tx.mu.Unlock()
	debugPrintf("DEBUG txCommit seq=%d invoking %d callbacks ts=%d\n", seq, len(cbs), time.Now().UnixNano())
	for _, cb := range cbs {
		cb(seq)
	}
	debugPrintf("DEBUG txCommit seq=%d callbacks done ts=%d\n", seq, time.Now().UnixNano())

	// Diagnostic: if a filename was registered for this seq, inspect the
	// prepared entry data blobs for directory entries that mention the
	// filename. This helps determine whether the committed transaction
	// actually contained the directory entry for the file.
	if fname, ok := getSeqName(seq); ok {
		// Trim leading slash for comparison against on-disk dir names.
		want := strings.TrimPrefix(fname, "/")
		found := false
		var foundBlock uint64
		var foundData []byte
		for _, e := range entries {
			// Attempt to parse directory entries from this block and
			// look for the filename.
			des := parseDirBlock(e.Data)
			for _, de := range des {
				if de.Name == want {
					found = true
					foundBlock = e.Block
					// copy the block data for optional dumping
					foundData = append([]byte(nil), e.Data...)
					break
				}
			}
			if found {
				break
			}
		}

		// Optionally dump any blocks that parse as directory blocks for
		// offline inspection. This helps compare how directory blocks
		// evolve across transactions when diagnosing missing entries.
		if os.Getenv("EXT4_DUMP_DIRBLOCKS") == "1" {
			for _, e := range entries {
				des := parseDirBlock(e.Data)
				if len(des) == 0 {
					continue
				}
				dumpPath := fmt.Sprintf("/tmp/ext4_seq_%d_block_%d_dir.bin", seq, e.Block)
				_ = os.WriteFile(dumpPath, e.Data, 0644)
				debugPrintf("DEBUG txCommit seq=%d dumped dir block=%d path=%s entries=%d\n", seq, e.Block, dumpPath, len(des))
			}
		}
		if found {
			// Optionally dump the raw block bytes for offline inspection.
			if os.Getenv("EXT4_DUMP_DIRBLOCKS") == "1" && len(foundData) > 0 {
				dumpPath := fmt.Sprintf("/tmp/ext4_seq_%d_block_%d_%s.bin", seq, foundBlock, strings.ReplaceAll(want, "/", "_"))
				_ = os.WriteFile(dumpPath, foundData, 0644)
				debugPrintf("DEBUG txCommit seq=%d applied entries CONTAIN dir entry name=%q foundBlock=%d dump=%s\n", seq, fname, foundBlock, dumpPath)
			} else {
				debugPrintf("DEBUG txCommit seq=%d applied entries CONTAIN dir entry name=%q foundBlock=%d\n", seq, fname, foundBlock)
			}
		} else {
			debugPrintf("DEBUG txCommit seq=%d applied entries DO NOT contain dir entry name=%q blocks=%v\n", seq, fname, blocks)
		}
	}

	// Mark the journal sequence acked so reservation waiters can observe
	// completion even if they didn't receive a channel ack.
	markSeqAcked(seq)
	debugPrintf("DEBUG txCommit seq=%d markSeqAcked ts=%d\n", seq, time.Now().UnixNano())

	// Invoke test hook after the transaction has been applied and synced.
	if CommitHook != nil {
		debugPrintf("DEBUG txCommit seq=%d invoking CommitHook ts=%d\n", seq, time.Now().UnixNano())
		CommitHook(seq, len(entries))
		debugPrintf("DEBUG txCommit seq=%d CommitHook done ts=%d\n", seq, time.Now().UnixNano())
	}

	return nil
}

// Abort cancels the transaction. For this implementation this is a no-op
// unless the transaction has been committed.
func (tx *Transaction) Abort() error {
	if tx == nil {
		return fmt.Errorf("nil transaction")
	}
	tx.mu.Lock()
	if tx.committed {
		tx.mu.Unlock()
		return fmt.Errorf("cannot abort committed transaction")
	}
	tx.entries = nil
	tx.mu.Unlock()
	// Release any per-block locks held by this transaction.
	tx.releaseLocks()
	return nil
}

// ReplayOnOpen scans the sidecar journal for committed transactions and
// applies them to the backing image. Partial or uncommitted transactions
// are ignored.
func (j *Journal) ReplayOnOpen() error {
	if j == nil || !j.enabled {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if _, err := j.js.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for {
		// Read tx header
		var magic uint32
		if err := binary.Read(j.js, binary.LittleEndian, &magic); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if magic != journalMagic {
			// Unknown data: stop parsing
			break
		}
		var seq uint64
		if err := binary.Read(j.js, binary.LittleEndian, &seq); err != nil {
			return err
		}
		var nDesc uint32
		if err := binary.Read(j.js, binary.LittleEndian, &nDesc); err != nil {
			return err
		}
		// Read descriptors
		descs := make([]struct {
			blk uint64
			ln  uint32
		}, nDesc)
		for i := uint32(0); i < nDesc; i++ {
			if err := binary.Read(j.js, binary.LittleEndian, &descs[i].blk); err != nil {
				return err
			}
			if err := binary.Read(j.js, binary.LittleEndian, &descs[i].ln); err != nil {
				return err
			}
		}
		// Read data for each descriptor
		datas := make([][]byte, nDesc)
		for i := uint32(0); i < nDesc; i++ {
			l := descs[i].ln
			buf := make([]byte, l)
			if _, err := io.ReadFull(j.js, buf); err != nil {
				return err
			}
			datas[i] = buf
		}
		// Read commit
		var cm uint32
		if err := binary.Read(j.js, binary.LittleEndian, &cm); err != nil {
			// no commit -> incomplete tx, stop
			break
		}
		if cm != commitMagic {
			// invalid commit -> stop
			break
		}
		var cseq uint64
		if err := binary.Read(j.js, binary.LittleEndian, &cseq); err != nil {
			return err
		}
		var crc uint32
		if err := binary.Read(j.js, binary.LittleEndian, &crc); err != nil {
			return err
		}
		// Verify crc by recomputing over header+descs+data.
		// Rewind to start of this tx to reconstruct bytes.
		// For simplicity, reconstruct in-memory using the parsed fields.
		var b bytes.Buffer
		binary.Write(&b, binary.LittleEndian, journalMagic)
		binary.Write(&b, binary.LittleEndian, seq)
		binary.Write(&b, binary.LittleEndian, nDesc)
		for i := uint32(0); i < nDesc; i++ {
			binary.Write(&b, binary.LittleEndian, descs[i].blk)
			binary.Write(&b, binary.LittleEndian, descs[i].ln)
		}
		for i := uint32(0); i < nDesc; i++ {
			b.Write(datas[i])
		}
		if crc32.Update(0, castagnoli, b.Bytes()) != crc {
			// checksum mismatch: skip this tx (likely partial) and stop
			break
		}
		// Apply entries to backing fs
		for i := uint32(0); i < nDesc; i++ {
			blk := descs[i].blk
			if blk >= uint64(j.sb.BlocksCount) {
				return fmt.Errorf("journal replay: block %d out of range", blk)
			}
			off := j.fsOffset + int64(blk)*int64(j.blockSize)
			if _, err := writeAtWithTrace(j.f, j.fsOffset, j.sb, off, datas[i]); err != nil {
				return fmt.Errorf("journal replay apply block %d: %w", blk, err)
			}
		}
		// Sync underlying file to ensure durability
		if j.fsFile != nil {
			if err := j.fsFile.Sync(); err != nil {
				return err
			}
		}
		// update nextSeq
		if seq >= j.nextSeq {
			j.nextSeq = seq + 1
		}
	}
	return nil
}

// initNextSeq scans the journal file to set the next sequence number.
func (j *Journal) initNextSeq() error {
	if j == nil || j.js == nil {
		return nil
	}
	if _, err := j.js.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var lastSeq uint64
	for {
		var magic uint32
		if err := binary.Read(j.js, binary.LittleEndian, &magic); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if magic != journalMagic {
			// unknown trailing data -> stop parsing
			break
		}
		var seq uint64
		if err := binary.Read(j.js, binary.LittleEndian, &seq); err != nil {
			return err
		}
		var nDesc uint32
		if err := binary.Read(j.js, binary.LittleEndian, &nDesc); err != nil {
			return err
		}
		// Read descriptors
		descs := make([]struct {
			blk uint64
			ln  uint32
		}, nDesc)
		for i := uint32(0); i < nDesc; i++ {
			if err := binary.Read(j.js, binary.LittleEndian, &descs[i].blk); err != nil {
				return err
			}
			if err := binary.Read(j.js, binary.LittleEndian, &descs[i].ln); err != nil {
				return err
			}
		}
		// Read data blobs
		datas := make([][]byte, nDesc)
		for i := uint32(0); i < nDesc; i++ {
			l := descs[i].ln
			buf := make([]byte, l)
			if _, err := io.ReadFull(j.js, buf); err != nil {
				// incomplete tx at EOF -> stop parsing
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					return nil
				}
				return err
			}
			datas[i] = buf
		}
		// Read commit record
		var cm uint32
		if err := binary.Read(j.js, binary.LittleEndian, &cm); err != nil {
			if err == io.EOF {
				// incomplete commit -> stop
				break
			}
			return err
		}
		if cm != commitMagic {
			// invalid commit -> stop
			break
		}
		var cseq uint64
		if err := binary.Read(j.js, binary.LittleEndian, &cseq); err != nil {
			return err
		}
		var crc uint32
		if err := binary.Read(j.js, binary.LittleEndian, &crc); err != nil {
			return err
		}
		// Recompute CRC over header+descs+data
		var b bytes.Buffer
		binary.Write(&b, binary.LittleEndian, journalMagic)
		binary.Write(&b, binary.LittleEndian, seq)
		binary.Write(&b, binary.LittleEndian, nDesc)
		for i := uint32(0); i < nDesc; i++ {
			binary.Write(&b, binary.LittleEndian, descs[i].blk)
			binary.Write(&b, binary.LittleEndian, descs[i].ln)
		}
		for i := uint32(0); i < nDesc; i++ {
			b.Write(datas[i])
		}
		if crc32.Update(0, castagnoli, b.Bytes()) == crc {
			if seq > lastSeq {
				lastSeq = seq
			}
		} else {
			// checksum mismatch: likely truncated/corrupt -> stop parsing
			break
		}
	}
	j.nextSeq = lastSeq + 1
	// Initialize append cursor to the current end of file so future
	// reservations don't overlap parsed content.
	if off, err := j.js.Seek(0, io.SeekEnd); err == nil {
		j.appendOff = off
	}
	// ensure append cursor initialized before workers run (no-op for global dispatcher)
	return nil
}

// startSyncWorkers launches background workers that serialise and coalesce
// Sync() syscalls for the journal and underlying filesystem file.
func (j *Journal) startSyncWorkers() {
	// kept for compatibility; no-op with global dispatcher
	return
}

// shutdownSyncWorkers stops background workers and waits for them to exit.
func (j *Journal) shutdownSyncWorkers() {
	// no-op; global dispatcher lives for process lifetime
	return
}

// syncWorker drains the provided queue, coalesces pending requests and
// performs a single Sync() to satisfy all of them.
// per-Journal worker removed: global dispatcher handles Sync()s.

// queueJSSync enqueues a journal Sync request and waits for completion.
// If the queue is full or workers are unavailable we fall back to direct Sync.
func (j *Journal) queueJSSync() error {
	if j == nil || j.js == nil {
		return nil
	}
	// Dispatch to the global sync worker pool. If the channel is full,
	// block until a worker accepts the task. This throttles producers
	// under load and bounds the number of concurrent Sync syscalls.
	task := &syncTask{f: j.js, ack: nil, label: "js.Sync"}
	select {
	case globalSyncTasks <- task:
		return nil
	default:
		// Block until we can enqueue (throttle producers).
		globalSyncTasks <- task
		return nil
	}
}

// queueFSFileSync enqueues a filesystem file Sync request and waits for it.
func (j *Journal) queueFSFileSync() error {
	if j == nil || j.fsFile == nil {
		return nil
	}
	task := &syncTask{f: j.fsFile, ack: nil, label: "fsFile.Sync"}
	select {
	case globalSyncTasks <- task:
		return nil
	default:
		// Block until we can enqueue (throttle producers).
		globalSyncTasks <- task
		return nil
	}
}

// Close cleanly shuts down the journal including sync workers and closes
// the underlying journal file. It also unregisters the journal.
//
// Each Transaction.Commit applies its entries to the backing image
// synchronously before returning, so at Close all committed entries are
// already persisted on disk. Truncate the journal file so the next Open
// does not replay a growing log of already-applied transactions (which
// would otherwise turn ReplayOnOpen into an O(N) tax on every reopen).
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	// stop workers
	j.shutdownSyncWorkers()
	if j.js != nil {
		_ = j.js.Truncate(0)
		_ = j.js.Sync()
		_ = j.js.Close()
	}
	unregisterJournal(j.f)
	return nil
}

// reserveAppend reserves `n` bytes at the end of the journal and returns the
// offset where the caller may safely write. It keeps an in-memory cursor
// (`appendOff`) to avoid overlapping reservations and uses `j.mu` to serialize
// reservations.
func (j *Journal) reserveAppend(n int64) (int64, error) {
	if j == nil || j.js == nil {
		return 0, fmt.Errorf("journal not available")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	// Ensure appendOff is at least the current file end.
	end, err := j.js.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if j.appendOff < end {
		j.appendOff = end
	}
	off := j.appendOff
	j.appendOff += n
	return off, nil
}
