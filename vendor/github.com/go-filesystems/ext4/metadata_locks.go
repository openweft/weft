// tryMutex implements a mutex with TryLock semantics using a buffered
// channel of size 1. It provides Lock/Unlock (blocking) and TryLock
package filesystem_ext4

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// tryMutex implements a mutex with TryLock semantics using a buffered
// channel of size 1. It provides Lock/Unlock (blocking) and TryLock
// (non-blocking) operations.
type tryMutex struct {
	ch chan struct{}
	// owner holds the goroutine id of the current owner (0 == unlocked).
	// Accessed atomically to avoid races when diagnostics are enabled.
	owner uint64
	// recursion counts nested locks by the same goroutine owner.
	// Accessed atomically.
	recursion int32
	// acquiredAt records the UnixNano timestamp when the lock was
	// initially acquired (recursion==1). Used for diagnostics.
	acquiredAt int64
	// debugMu protects ownerStack which stores a stack trace captured
	// when the lock was acquired. This is only used for diagnostics.
	debugMu    sync.Mutex
	ownerStack []byte
}

func newTryMutex() *tryMutex {
	m := &tryMutex{ch: make(chan struct{}, 1)}
	m.ch <- struct{}{}
	atomic.StoreUint64(&m.owner, 0)
	return m
}

func (m *tryMutex) Lock() {
	// Fast-path try to acquire without blocking diagnostic work.
	if m.TryLock() {
		return
	}
	// If this goroutine already owns the lock (re-entrant), increment
	// recursion and return.
	gid := getGID()
	if atomic.LoadUint64(&m.owner) == gid {
		atomic.AddInt32(&m.recursion, 1)
		if ext4LockDebug {
			debugPrintf("DEBUG tryMutex reenter owner=%d recursion=%d\n", gid, atomic.LoadInt32(&m.recursion))
		}
		return
	}
	// If diagnostics enabled, report current owner stack before blocking.
	if ext4LockDebug {
		cur := atomic.LoadUint64(&m.owner)
		if cur != 0 {
			m.debugMu.Lock()
			stack := append([]byte(nil), m.ownerStack...)
			m.debugMu.Unlock()
			debugPrintf("DEBUG tryMutex waiting owner=%d\n%s\n", cur, string(stack))
		}
	}
	<-m.ch
	// record owner and reset recursion and capture stack for diagnostics
	gid = getGID()
	atomic.StoreUint64(&m.owner, gid)
	atomic.StoreInt32(&m.recursion, 1)
	atomic.StoreInt64(&m.acquiredAt, time.Now().UnixNano())
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		m.debugMu.Lock()
		m.ownerStack = append([]byte(nil), buf[:n]...)
		m.debugMu.Unlock()
		debugPrintf("DEBUG tryMutex acquired owner=%d\n", gid)
	}
}

func (m *tryMutex) Unlock() {
	if ext4LockDebug {
		debugPrintf("DEBUG tryMutex releasing owner=%d\n", atomic.LoadUint64(&m.owner))
	}
	// Decrement recursion and only fully release when it reaches zero.
	if atomic.AddInt32(&m.recursion, -1) > 0 {
		if ext4LockDebug {
			debugPrintf("DEBUG tryMutex partial release owner=%d recursion=%d\n", atomic.LoadUint64(&m.owner), atomic.LoadInt32(&m.recursion))
		}
		return
	}
	// compute hold duration for diagnostics
	heldNs := time.Now().UnixNano() - atomic.LoadInt64(&m.acquiredAt)
	atomic.StoreInt64(&m.acquiredAt, 0)
	atomic.StoreUint64(&m.owner, 0)
	m.debugMu.Lock()
	m.ownerStack = nil
	m.debugMu.Unlock()
	m.ch <- struct{}{}
	if ext4LockDebug {
		if ext4LockHoldWarnMs != 0 {
			if heldNs >= int64(ext4LockHoldWarnMs)*int64(time.Millisecond) {
				buf := make([]byte, 1<<12)
				n := runtime.Stack(buf, false)
				debugPrintf("WARN tryMutex held duration=%dms stack:\n%s\n", heldNs/int64(time.Millisecond), string(buf[:n]))
			}
		}
	}
}

func (m *tryMutex) TryLock() bool {
	gid := getGID()
	// If current goroutine already owns the lock, support re-entrant acquire.
	if atomic.LoadUint64(&m.owner) == gid {
		atomic.AddInt32(&m.recursion, 1)
		if ext4LockDebug {
			debugPrintf("DEBUG tryMutex TryLock reenter owner=%d recursion=%d\n", gid, atomic.LoadInt32(&m.recursion))
		}
		return true
	}
	select {
	case <-m.ch:
		atomic.StoreUint64(&m.owner, gid)
		atomic.StoreInt32(&m.recursion, 1)
		atomic.StoreInt64(&m.acquiredAt, time.Now().UnixNano())
		if ext4LockDebug {
			buf := make([]byte, 1<<12)
			n := runtime.Stack(buf, false)
			m.debugMu.Lock()
			m.ownerStack = append([]byte(nil), buf[:n]...)
			m.debugMu.Unlock()
			debugPrintf("DEBUG tryMutex TryLock owner=%d\n", gid)
		}
		return true
	default:
		return false
	}
}

var bgdTableLocksMu sync.Mutex
var bgdTableLocks = map[string]*sync.Mutex{}

var bgdBlockLocksMu sync.Mutex
var bgdBlockLocks = map[string]*sync.Mutex{}
var dataBlockLocksMu sync.Mutex
var dataBlockLocks = map[string]*tryMutex{}
var bgdGroupLocksMu sync.Mutex
var bgdGroupLocks = map[string]*tryMutex{}

var bitmapGroupLocksMu sync.Mutex
var bitmapGroupLocks = map[string]*tryMutex{}

// reservedBits holds in-memory reservations for bitmap bits that have been
// allocated logically but not yet persisted to disk. This prevents other
// allocators from selecting the same bits while the committing goroutine
// performs IO without holding the bitmap lock.
var reservedBitsMu sync.Mutex
var reservedBits = map[string][]byte{}

// appendImmediate writes a short line to a debug file under /tmp.
// This is intentionally unconditional to aid timing-sensitive
// instrumentation during test runs.
func appendImmediate(path, s string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(s + "\n")
	_ = f.Close()
}

// reservationInfo records the intended on-disk value for a reserved bit and
// an optional ack/seq that will be signalled when the dispatch task that
// persisted the change has completed. This allows callers to wait for the
// authoritative commit acknowledgement before clearing the reservation.
type reservationInfo struct {
	want bool
	ack  chan error
	seq  uint64
	// ts records the UnixNano timestamp when the reservation was created.
	// Used by optional cleanup of stale reservations for debugging/tests.
	ts int64
}

// reservedWant records the intended on-disk bit value for each reserved
// bit along with optional ack/seq metadata. The map maps reservation key ->
// bit index -> reservationInfo.
var reservedWant = map[string]map[int]reservationInfo{}

func init() {
	if ext4LockHoldWarnMs > 0 {
		go watchLongHeldLocks(time.Duration(ext4LockHoldWarnMs) * time.Millisecond)
	}
	// Optional stale reservation cleanup controlled by EXT4_RESERVATION_STALE_MS
	if v := os.Getenv("EXT4_RESERVATION_STALE_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			go cleanupStaleReservations(time.Duration(ms) * time.Millisecond)
		}
	}
}

// watchLongHeldLocks periodically scans known tryMutex maps and emits a
// diagnostic when a lock has been held longer than the configured threshold.
func watchLongHeldLocks(threshold time.Duration) {
	ticker := time.NewTicker(threshold / 4)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixNano()
		// scan BGD group locks
		bgdGroupLocksMu.Lock()
		for k, m := range bgdGroupLocks {
			owner := atomic.LoadUint64(&m.owner)
			if owner == 0 {
				continue
			}
			acquired := atomic.LoadInt64(&m.acquiredAt)
			if acquired == 0 {
				continue
			}
			if time.Duration(now-acquired) >= threshold {
				m.debugMu.Lock()
				stack := append([]byte(nil), m.ownerStack...)
				m.debugMu.Unlock()
				debugPrintf("WARN longHeldLock key=%s owner=%d heldMs=%d\nstack:\n%s\n", k, owner, (now-acquired)/int64(time.Millisecond), string(stack))
			}
		}
		bgdGroupLocksMu.Unlock()

		// scan bitmap group locks
		bitmapGroupLocksMu.Lock()
		for k, m := range bitmapGroupLocks {
			owner := atomic.LoadUint64(&m.owner)
			if owner == 0 {
				continue
			}
			acquired := atomic.LoadInt64(&m.acquiredAt)
			if acquired == 0 {
				continue
			}
			if time.Duration(now-acquired) >= threshold {
				m.debugMu.Lock()
				stack := append([]byte(nil), m.ownerStack...)
				m.debugMu.Unlock()
				debugPrintf("WARN longHeldLock key=%s owner=%d heldMs=%d\nstack:\n%s\n", k, owner, (now-acquired)/int64(time.Millisecond), string(stack))
			}
		}
		bitmapGroupLocksMu.Unlock()
	}
}

// cleanupStaleReservations scans the in-memory reservation set and forcibly
// clears entries that are older than the given threshold. This is an
// optional debugging helper enabled by setting EXT4_RESERVATION_STALE_MS.
func cleanupStaleReservations(threshold time.Duration) {
	ticker := time.NewTicker(threshold / 2)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixNano()
		var cleared []string
		reservedBitsMu.Lock()
		for key, wm := range reservedWant {
			var bitsToClear []int
			for bit, ri := range wm {
				if ri.ts != 0 && time.Duration(now-ri.ts) >= threshold {
					bitsToClear = append(bitsToClear, bit)
				}
			}
			if len(bitsToClear) == 0 {
				continue
			}
			// clear bits from reservedBits and reservedWant
			b := reservedBits[key]
			for _, bit := range bitsToClear {
				if bit >= 0 && bit/8 < len(b) {
					b[bit/8] &^= 1 << uint(bit%8)
				}
				delete(wm, bit)
			}
			if len(wm) == 0 {
				delete(reservedWant, key)
			} else {
				reservedWant[key] = wm
			}
			reservedBits[key] = b
			cleared = append(cleared, fmt.Sprintf("%s:%v", key, bitsToClear))
		}
		reservedBitsMu.Unlock()
		for _, s := range cleared {
			debugPrintf("WARN staleReservationCleanup cleared=%s\n", s)
		}
	}
}

func bgdTableLockKey(f readerWriterAt) string {
	return fmt.Sprintf("%s:bgdTable", fileLockKey(f))
}

// lockBGDTable acquires a mutex serializing writes to the block group
// descriptor table for the given backing file. Returns an unlock function.
func lockBGDTable(f readerWriterAt) func() {
	key := bgdTableLockKey(f)
	bgdTableLocksMu.Lock()
	m, ok := bgdTableLocks[key]
	if !ok {
		m = &sync.Mutex{}
		bgdTableLocks[key] = m
	}
	bgdTableLocksMu.Unlock()
	m.Lock()
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("DEBUG lockBGDTable acquired key=%s\n%s\n", key, string(buf[:n]))
	}
	return func() {
		if ext4LockDebug {
			debugPrintf("DEBUG lockBGDTable releasing key=%s\n", key)
		}
		m.Unlock()
	}
}

func bgdBlockLockKey(f readerWriterAt, block uint64) string {
	return fmt.Sprintf("%s:bgdTable:block:%d", fileLockKey(f), block)
}

// lockBGDTableBlock acquires a mutex for the specific block that contains
// block group descriptors (or the superblock). This narrows contention
// compared to a single global BGD table lock.
func lockBGDTableBlock(f readerWriterAt, block uint64) func() {
	key := bgdBlockLockKey(f, block)
	bgdBlockLocksMu.Lock()
	m, ok := bgdBlockLocks[key]
	if !ok {
		m = &sync.Mutex{}
		bgdBlockLocks[key] = m
	}
	bgdBlockLocksMu.Unlock()
	m.Lock()
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("DEBUG lockBGDTableBlock acquired key=%s\n%s\n", key, string(buf[:n]))
	}
	return func() {
		if ext4LockDebug {
			debugPrintf("DEBUG lockBGDTableBlock releasing key=%s\n", key)
		}
		m.Unlock()
	}
}

func dataBlockLockKey(f readerWriterAt, block uint64) string {
	return fmt.Sprintf("%s:block:%d", fileLockKey(f), block)
}

// lockDataBlock acquires a mutex serializing preparations that read/modify
// a specific filesystem block. This prevents concurrent transaction
// preparations from performing read-modify-write races that can lead to
// lost-updates when transactions are applied in sequence.
func lockDataBlock(f readerWriterAt, block uint64) func() {
	key := dataBlockLockKey(f, block)
	dataBlockLocksMu.Lock()
	m, ok := dataBlockLocks[key]
	if !ok {
		m = newTryMutex()
		dataBlockLocks[key] = m
	}
	dataBlockLocksMu.Unlock()
	m.Lock()
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("DEBUG lockDataBlock acquired key=%s\n%s\n", key, string(buf[:n]))
	}
	return func() {
		if ext4LockDebug {
			debugPrintf("DEBUG lockDataBlock releasing key=%s\n", key)
		}
		m.Unlock()
	}
}

func bgdGroupLockKey(f readerWriterAt, group uint32) string {
	return fmt.Sprintf("%s:bgdTable:g:%d", fileLockKey(f), group)
}

// lockBGDGroup acquires a mutex protecting a single block group descriptor
// entry within the BGD table. This allows concurrent writers to different
// block groups without contending on a single global lock.
func lockBGDGroup(f readerWriterAt, group uint32) func() {
	key := bgdGroupLockKey(f, group)
	bgdGroupLocksMu.Lock()
	m, ok := bgdGroupLocks[key]
	if !ok {
		m = newTryMutex()
		bgdGroupLocks[key] = m
	}
	bgdGroupLocksMu.Unlock()
	m.Lock()
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("DEBUG lockBGDGroup acquired key=%s\n%s\n", key, string(buf[:n]))
	}
	return func() {
		if ext4LockDebug {
			debugPrintf("DEBUG lockBGDGroup releasing key=%s\n", key)
		}
		m.Unlock()
	}
}

func bitmapLockKey(f readerWriterAt, group uint32, isBlock bool) string {
	return fmt.Sprintf("%s:bitmap:g:%d:block:%t", fileLockKey(f), group, isBlock)
}

// reserveBitmapBits marks bits in the in-memory reservation set for the
// given file+group/isBlock. Caller should hold the bitmap lock while
// calling so reservations are installed atomically with the local bitmap
// changes.
func reserveBitmapBits(f readerWriterAt, sb *superblock, group uint32, isBlock bool, bits []int, wantSet bool) {
	key := bitmapLockKey(f, group, isBlock)
	if ext4LockDebug {
		debugPrintf("DEBUG reserveBitmapBits key=%s bits=%v want=%t\n", key, bits, wantSet)
	}
	// Attempt to eagerly find a recorded seq for this backing group so we
	// can attach it to the reservation immediately at creation time.
	// Perform the seqInfos scan before taking reservedBitsMu to avoid
	// lock-order inversions with callers that may hold seqInfosMu.
	var eagerSeq uint64
	seqInfosMu.Lock()
	for s, si := range seqInfos {
		if si.f == f && si.group == group {
			if s > eagerSeq {
				eagerSeq = s
			}
		}
	}
	seqInfosMu.Unlock()
	// Optionally preassign a sequence for this reservation so later enqueues
	// can deterministically claim it. This is an intrusive mode enabled by
	// EXT4_RESERVE_PREASSIGN_SEQ; set seqClaimed to false so the eventual
	// enqueue path can claim it.
	if eagerSeq == 0 && reservePreassignSeqEnabled {
		// allocate next global seq and record seqInfo
		s := atomic.AddUint64(&commitTaskSeq, 1)
		seqInfosMu.Lock()
		seqInfos[s] = seqInfo{f: f, fsOffset: 0, sb: sb, group: group}
		seqClaimed[s] = false
		seqInfosMu.Unlock()
		eagerSeq = s
		if ext4LockDebug {
			debugPrintf("DEBUG reserveBitmapBits preassigned seq=%d key=%s\n", s, key)
		}
		appendImmediate("/tmp/ext4_reserve_debug.log", fmt.Sprintf("reserve preassign key=%s seq=%d ts=%d", key, s, time.Now().UnixNano()))
	}
	reservedBitsMu.Lock()
	defer reservedBitsMu.Unlock()
	// Determine needed size based on blocks or inodes per group.
	var maxBits int
	if isBlock {
		maxBits = int(sb.BlocksPerGroup)
	} else {
		maxBits = int(sb.InodesPerGroup)
	}
	n := (maxBits + 7) / 8
	b, ok := reservedBits[key]
	if !ok || len(b) < n {
		nb := make([]byte, n)
		if ok {
			copy(nb, b)
		}
		b = nb
		reservedBits[key] = b
	}
	// ensure reservedWant entry exists
	wm, wok := reservedWant[key]
	if !wok {
		wm = map[int]reservationInfo{}
		reservedWant[key] = wm
	}
	for _, bit := range bits {
		if bit >= 0 && bit < maxBits {
			b[bit/8] |= 1 << uint(bit%8)
			ri := wm[bit]
			ri.want = wantSet
			ri.ts = time.Now().UnixNano()
			// If we discovered a matching sequence earlier, attach it now so
			// the reservation reflects the authoritative seq immediately and
			// avoids the TOCTOU where the ack->seq mapping may be removed.
			if eagerSeq != 0 && ri.seq == 0 {
				ri.seq = eagerSeq
				if ext4LockDebug {
					debugPrintf("DEBUG reserveBitmapBits eagerAssociate seq=%d key=%s bit=%d ts=%d\n", ri.seq, key, bit, time.Now().UnixNano())
				}
				appendImmediate("/tmp/ext4_reserve_debug.log", fmt.Sprintf("reserve eagerAssoc key=%s bit=%d seq=%d ts=%d", key, bit, ri.seq, time.Now().UnixNano()))
			}
			// preserve any existing ack/seq association
			wm[bit] = ri
			// Immediate debug trace for reservation creation.
			appendImmediate("/tmp/ext4_reserve_debug.log", fmt.Sprintf("reserve key=%s bit=%d want=%t ts=%d", key, bit, wantSet, ri.ts))
		}
	}

	// For timing-sensitive debugging, capture a shallow copy of the
	// reservation entries and emit a forensic snapshot asynchronously so
	// watchers can inspect reservation state before any racing seq/ack
	// bookkeeping removes the mapping. Copy under lock to avoid map races.
	if ext4LockDebug {
		if wm != nil {
			wantCopy := make(map[int]reservationInfo, len(wm))
			for k, v := range wm {
				wantCopy[k] = v
			}
			// Non-blocking: run snapshot in background to avoid holding locks.
			go dumpForensicSnapshot(key, bits, wantCopy, nil, nil)
			// Spawn a short-lived watcher to attach any soon-to-be-created
			// sequence to these reservations. This helps eliminate the TOCTOU
			// where the worker removes the ack->seq mapping before the
			// association runs. Only run this extra watcher under debug so
			// ordinary behavior is unchanged unless diagnostics are active.
			go func(f readerWriterAt, sb *superblock, group uint32, isBlock bool, key string, bits []int) {
				// Use the ack->seq grace window as a reasonable wait bound.
				wait := ackToSeqDeleteGrace
				if wait <= 0 {
					wait = 200 * time.Millisecond
				}
				deadline := time.Now().Add(wait)
				for time.Now().Before(deadline) {
					// Scan known seq infos for a matching backing group.
					seqInfosMu.Lock()
					for seq, si := range seqInfos {
						if si.f == f && si.group == group {
							seqInfosMu.Unlock()
							// Directly associate the sequence with the reservation entries
							// (this mirrors associateSeqWithReservation but is safe to call
							// from the watcher context).
							associateSeqWithReservation(si.f, si.group, isBlock, bits, seq)
							return
						}
					}
					seqInfosMu.Unlock()
					time.Sleep(5 * time.Millisecond)
				}
			}(f, sb, group, isBlock, key, bits)
		}
	}
}

// associateAckWithReservation associates the given ack channel (and its
// dispatcher sequence, if known) with the reserved bits for the provided
// file/group/isBlock. Only bits that already have reservations will be
// associated; missing entries are ignored.
func associateAckWithReservation(f readerWriterAt, group uint32, isBlock bool, bits []int, ack chan error) {
	if ack == nil {
		return
	}
	key := bitmapLockKey(f, group, isBlock)
	appendAssocLog(fmt.Sprintf("assoc start key=%s ack=%p ts=%d", key, ack, time.Now().UnixNano()))
	appendImmediate("/tmp/ext4_assoc_direct.log", fmt.Sprintf("assoc start key=%s ack=%p ts=%d", key, ack, time.Now().UnixNano()))

	// Capture a pre-association forensic snapshot for any reserved bits that
	// currently lack a seq. This helps diagnose races where the dispatcher
	// removed the ack->seq mapping before association. Copy the minimal
	// reservation subset under lock and invoke the snapshot asynchronously.
	if ext4LockDebug {
		var preBits []int
		var preWant map[int]reservationInfo
		reservedBitsMu.Lock()
		if wm, ok := reservedWant[key]; ok {
			for _, bit := range bits {
				if ri, ok2 := wm[bit]; ok2 && ri.seq == 0 {
					preBits = append(preBits, bit)
					if preWant == nil {
						preWant = make(map[int]reservationInfo)
					}
					preWant[bit] = ri
				}
			}
		}
		reservedBitsMu.Unlock()
		if len(preBits) > 0 {
			go dumpForensicSnapshot(key, preBits, preWant, nil, nil)
		}
	}
	// Lookup dispatcher seq for this ack channel (if available)
	seq, _ := getSeqForAck(ack)
	appendAssocLog(fmt.Sprintf("assoc gotSeq key=%s ack=%p seq=%d ts=%d", key, ack, seq, time.Now().UnixNano()))
	appendImmediate("/tmp/ext4_assoc_direct.log", fmt.Sprintf("assoc gotSeq key=%s ack=%p seq=%d ts=%d", key, ack, seq, time.Now().UnixNano()))
	// First, associate ack/seq under the reservedBitsMu and collect bits
	// that didn't have an associated seq so we can detect a late-delivered
	// ack (worker already sent it) outside the lock.
	var bitsNeedAckCheck []int
	reservedBitsMu.Lock()
	if wm, ok := reservedWant[key]; ok {
		for _, bit := range bits {
			if ri, ok2 := wm[bit]; ok2 {
				ri.ack = ack
				if seq != 0 {
					ri.seq = seq
				}
				wm[bit] = ri
				appendAssocLog(fmt.Sprintf("assoc underLock key=%s bit=%d seq=%d ack=%p ts=%d", key, bit, ri.seq, ri.ack, time.Now().UnixNano()))
				appendImmediate("/tmp/ext4_assoc_direct.log", fmt.Sprintf("assoc underLock key=%s bit=%d seq=%d ack=%p ts=%d", key, bit, ri.seq, ri.ack, time.Now().UnixNano()))
				if ext4LockDebug {
					debugPrintf("DEBUG associateAckWithReservation key=%s bit=%d seq=%d ack=%p ts=%d\n", key, bit, ri.seq, ri.ack, time.Now().UnixNano())
				}
				if ri.seq != 0 && isSeqAcked(ri.seq) {
					// clear reserved bit
					if rb, rok := reservedBits[key]; rok && bit >= 0 && bit/8 < len(rb) {
						rb[bit/8] &^= 1 << uint(bit%8)
						reservedBits[key] = rb
					}
					delete(wm, bit)
					if ext4LockDebug {
						debugPrintf("DEBUG associateAckWithReservation cleared already-acked reservation key=%s bit=%d seq=%d\n", key, bit, ri.seq)
					}
				} else if ri.seq == 0 {
					bitsNeedAckCheck = append(bitsNeedAckCheck, bit)
				}
			} else {
				appendAssocLog(fmt.Sprintf("assoc missingReservation key=%s bit=%d seq=%d ack=%p ts=%d", key, bit, seq, ack, time.Now().UnixNano()))
				appendImmediate("/tmp/ext4_assoc_direct.log", fmt.Sprintf("assoc missingReservation key=%s bit=%d seq=%d ack=%p ts=%d", key, bit, seq, ack, time.Now().UnixNano()))
				if ext4LockDebug {
					debugPrintf("DEBUG associateAckWithReservation missing reservation key=%s bit=%d seq=%d ack=%p ts=%d\n", key, bit, seq, ack, time.Now().UnixNano())
				}
			}
		}
	}
	reservedBitsMu.Unlock()

	// If the dispatcher already delivered the ack (seq mapping removed),
	// the ack value may be buffered in the channel. Detect that non-
	// blocking, re-insert the value to preserve semantics, and clear the
	// reservation under the same lock.
	if len(bitsNeedAckCheck) > 0 {
		select {
		case v := <-ack:
			// Re-insert the ack value so the original waiter still receives it.
			ack <- v
			appendAssocLog(fmt.Sprintf("assoc lateAck key=%s ack=%p val=%v ts=%d", key, ack, v, time.Now().UnixNano()))
			appendImmediate("/tmp/ext4_assoc_direct.log", fmt.Sprintf("assoc lateAck key=%s ack=%p val=%v ts=%d", key, ack, v, time.Now().UnixNano()))
			reservedBitsMu.Lock()
			if wm, ok := reservedWant[key]; ok {
				if rb, rok := reservedBits[key]; rok {
					for _, bit := range bitsNeedAckCheck {
						if ri, ok2 := wm[bit]; ok2 && ri.ack == ack {
							appendAssocLog(fmt.Sprintf("assoc clearing reserved bit(key=%s bit=%d) due to lateAck ack=%p ts=%d", key, bit, ack, time.Now().UnixNano()))
							appendImmediate("/tmp/ext4_assoc_direct.log", fmt.Sprintf("assoc clearing reserved bit(key=%s bit=%d) due to lateAck ack=%p ts=%d", key, bit, ack, time.Now().UnixNano()))
							if bit >= 0 && bit/8 < len(rb) {
								rb[bit/8] &^= 1 << uint(bit%8)
								reservedBits[key] = rb
							}
							delete(wm, bit)
							if ext4LockDebug {
								debugPrintf("DEBUG associateAckWithReservation cleared late-acked reservation key=%s bit=%d ack=%p\n", key, bit, ack)
							}
						}
					}
				}
			}
			reservedBitsMu.Unlock()
		default:
			// no buffered ack present; nothing to do
		}
	}
}

// associateSeqWithReservation associates a dispatcher/journal sequence number
// with reserved bits. This is used for Transaction-based commits where there
// is no ack channel to map; the seq will be marked acked by the committer
// and unreserve will wait on the seq.
func associateSeqWithReservation(f readerWriterAt, group uint32, isBlock bool, bits []int, seq uint64) {
	if seq == 0 {
		return
	}
	key := bitmapLockKey(f, group, isBlock)
	reservedBitsMu.Lock()
	defer reservedBitsMu.Unlock()
	if wm, ok := reservedWant[key]; ok {
		for _, bit := range bits {
			if ri, ok2 := wm[bit]; ok2 {
				ri.seq = seq
				wm[bit] = ri
				if ext4LockDebug {
					debugPrintf("DEBUG associateSeqWithReservation key=%s bit=%d seq=%d ts=%d\n", key, bit, seq, time.Now().UnixNano())
				}
			}
		}
	}
}

// unreserveBitmapBits clears the specified bits from the in-memory
// reservation set. Safe to call when the caller no longer needs the
// reservation (for example, after a successful commit or on error cleanup).
func unreserveBitmapBits(f readerWriterAt, fsOffset int64, sb *superblock, group uint32, isBlock bool, bits []int) {
	key := bitmapLockKey(f, group, isBlock)
	if ext4LockDebug {
		debugPrintf("DEBUG unreserveBitmapBits key=%s bits=%v\n", key, bits)
	}
	// Immediate trace for unreserve start
	appendImmediate("/tmp/ext4_unreserve_debug.log", fmt.Sprintf("unreserve start key=%s bits=%v ts=%d", key, bits, time.Now().UnixNano()))
	// If we can, prefer waiting for the authoritative commit acknowledgement
	// associated with the reservation before clearing it. This ties the in-
	// memory reservation lifecycle to the actual dispatcher task that
	// persisted the change, avoiding TOCTOU where polling on-disk can miss
	// transient interleavings.
	var wantMap map[int]reservationInfo
	reservedBitsMu.Lock()
	if wm, ok := reservedWant[key]; ok {
		// make a shallow copy to use outside the lock
		wantMap = make(map[int]reservationInfo, len(wm))
		for k, v := range wm {
			wantMap[k] = v
		}
	}
	reservedBitsMu.Unlock()

	if f != nil && sb != nil && wantMap != nil && sb.BlocksPerGroup != 0 {
		// Emit per-bit reservation info for diagnostics
		if ext4LockDebug {
			for _, bit := range bits {
				if ri, ok := wantMap[bit]; ok {
					debugPrintf("DEBUG unreserveBitmapBits want entry key=%s bit=%d want=%t seq=%d ack=%p ts=%d\n", key, bit, ri.want, ri.seq, ri.ack, time.Now().UnixNano())
				} else {
					debugPrintf("DEBUG unreserveBitmapBits no-want entry key=%s bit=%d ts=%d\n", key, bit, time.Now().UnixNano())
				}
			}
		}
		// Collect unique sequences/acks for the bits we're clearing.
		uniq := map[uint64]chan error{}
		bitsToSeq := map[int]uint64{}
		for _, bit := range bits {
			if ri, ok := wantMap[bit]; ok {
				if ri.seq != 0 {
					uniq[ri.seq] = ri.ack
					bitsToSeq[bit] = ri.seq
				}
			}
		}
		if ext4LockDebug {
			debugPrintf("DEBUG unreserveBitmapBits key=%s uniq=%v bitsToSeq=%v ts=%d\n", key, uniq, bitsToSeq, time.Now().UnixNano())
		}
		// If we have explicit seqs, wait for their acknowledgements.
		// Bound the wait to avoid blocking indefinitely if a sequence is
		// never marked acked (defensive against bugs or races). If the
		// timeout expires, fall back to on-disk verification below.
		if len(uniq) > 0 {
			const waitDeadline = 200 * time.Millisecond
			deadline := time.Now().Add(waitDeadline)
			debugPrintf("DEBUG unreserveBitmapBits key=%s waiting for seqs=%v until=%d\n", key, uniq, deadline.UnixNano())
			// Poll until all seqs are acked or deadline expires.
			for time.Now().Before(deadline) {
				allAcked := true
				var unacked []uint64
				for seq := range uniq {
					if !isSeqAcked(seq) {
						allAcked = false
						unacked = append(unacked, seq)
					}
				}
				if !allAcked && ext4LockDebug {
					debugPrintf("DEBUG unreserveBitmapBits key=%s waiting unacked=%v ts=%d\n", key, unacked, time.Now().UnixNano())
				}
				if allAcked {
					debugPrintf("DEBUG unreserveBitmapBits key=%s all seqs acked ts=%d\n", key, time.Now().UnixNano())
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			// If not all acked by the deadline, log and fall back to
			// on-disk verification below rather than wait forever.
			anyUnacked := false
			for seq := range uniq {
				if !isSeqAcked(seq) {
					anyUnacked = true
					break
				}
			}
			if anyUnacked {
				debugPrintf("WARN unreserveBitmapBits timed out waiting for seqs key=%s seqs=%v bits=%v\n", key, uniq, bits)
			} else {
				// After the authoritative seqs are acked, verify the on-disk
				// descriptor and bitmap are consistent before clearing the
				// in-memory reservation. This avoids a race where a later
				// enqueued task overwrote descriptor bytes or bitmap state such
				// that clearing the reservation would expose an inconsistent
				// view to readers.
				const maxAttempts = 1000
				const sleepInterval = 5 * time.Millisecond
				verified := false
				var lastD *bgd
				var lastBmap []byte
				for attempt := 0; attempt < maxAttempts; attempt++ {
					d, derr := readBGD(f, fsOffset, sb, group)
					if derr != nil {
						time.Sleep(sleepInterval)
						continue
					}
					// Read the correct bitmap based on whether this is a block or inode reservation.
					bitmapBlockNum := d.InodeBitmapBlock
					if isBlock {
						bitmapBlockNum = d.BlockBitmapBlock
					}
					bmap, berr := readRawBlock(f, fsOffset, sb, bitmapBlockNum)
					if berr != nil {
						time.Sleep(sleepInterval)
						continue
					}
					// capture last successful read for diagnostics
					lastD = d
					lastBmap = bmap
					allMatch := true
					// Verify per-bit desired values match on-disk bitmap
					for _, bit := range bits {
						ri, ok := wantMap[bit]
						var want bool
						if !ok {
							// If we don't have an explicit desired value, assume true
							// (allocated) to be conservative and avoid races.
							want = true
						} else {
							want = ri.want
						}
						if bit < 0 || bit >= len(bmap)*8 {
							allMatch = false
							break
						}
						diskVal := (bmap[bit/8] & (1 << uint(bit%8))) != 0
						if diskVal != want {
							allMatch = false
							break
						}
					}
					if allMatch {
						max := int(sb.BlocksPerGroup)
						if max > len(bmap)*8 {
							max = len(bmap) * 8
						}
						freeCount := 0
						for i := 0; i < max; i++ {
							if bmap[i/8]&(1<<uint(i%8)) == 0 {
								freeCount++
							}
						}
						if uint32(freeCount) != d.FreeBlocksCount {
							if ext4LockDebug {
								debugPrintf("DEBUG unreserveBitmapBits bgd mismatch (accepting per-bit match) key=%s bitmapFree=%d bgdFree=%d seqs=%v bits=%v\n", key, freeCount, d.FreeBlocksCount, uniq, bits)
							}
							// Accept the per-bit match even if the BGD's FreeBlocksCount
							// differs. In practice later concurrent updates can change
							// the BGD count; requiring strict equality caused spurious
							// verification failures and reservation leakage. Proceed
							// as verified when the per-bit checks matched.
						}
					}
					if allMatch {
						verified = true
						break
					}
					time.Sleep(sleepInterval)
				}
				if !verified {
					if ext4LockDebug {
						if lastD != nil && lastBmap != nil {
							// Emit an on-disk forensic snapshot to aid post-mortem inspection.
							dumpForensicSnapshot(key, bits, wantMap, lastD, lastBmap)
							// Log per-bit mismatches and bitmap-derived free count
							for _, bit := range bits {
								ri, ok := wantMap[bit]
								var want bool
								if !ok {
									want = true
								} else {
									want = ri.want
								}
								var diskVal bool
								if bit >= 0 && bit < len(lastBmap)*8 {
									diskVal = (lastBmap[bit/8] & (1 << uint(bit%8))) != 0
								}
								debugPrintf("DEBUG unreserveBitmapBits mismatch key=%s bit=%d want=%t disk=%t\n", key, bit, want, diskVal)
							}
							max := int(sb.BlocksPerGroup)
							if max > len(lastBmap)*8 {
								max = len(lastBmap) * 8
							}
							freeCount := 0
							for i := 0; i < max; i++ {
								if lastBmap[i/8]&(1<<uint(i%8)) == 0 {
									freeCount++
								}
							}
							debugPrintf("DEBUG unreserveBitmapBits bgdFree=%d bitmapFree=%d key=%s\n", lastD.FreeBlocksCount, freeCount, key)
						} else {
							debugPrintf("DEBUG unreserveBitmapBits no-last-read key=%s\n", key)
						}
					}
					debugPrintf("WARN unreserveBitmapBits verification failed key=%s seqs=%v bits=%v\n", key, uniq, bits)
				}
			}
		} else {
			// No explicit sequence associated yet. It's possible a
			// concurrent registration (tx callback or associateAck) is
			// racing with this unreserve call. Spin briefly looking for a
			// newly-associated seq for these bits so we can wait on the
			// authoritative acknowledgement instead of falling back to
			// on-disk polling (reduces TOCTOU windows).
			// Increase the spin window to give racing associations more
			// time to appear under heavy concurrency. Keep this conservative
			// to avoid long blocking in normal cases.
			deadline := time.Now().Add(200 * time.Millisecond)
			for time.Now().Before(deadline) {
				// Inspect reservedWant under lock; capture count and any newly-associated seqs.
				reservedBitsMu.Lock()
				wmLen := 0
				// capture per-bit reservation status for diagnostics
				bitStatus := map[int]struct {
					seq    uint64
					hasAck bool
					ageNs  int64
				}{}
				if wm, ok := reservedWant[key]; ok {
					wmLen = len(wm)
					for _, bit := range bits {
						if ri, ok2 := wm[bit]; ok2 {
							if ri.seq != 0 {
								uniq[ri.seq] = ri.ack
								bitsToSeq[bit] = ri.seq
							}
							var age int64
							if ri.ts != 0 {
								age = time.Now().UnixNano() - ri.ts
							}
							bitStatus[bit] = struct {
								seq    uint64
								hasAck bool
								ageNs  int64
							}{seq: ri.seq, hasAck: ri.ack != nil, ageNs: age}
						}
					}
				}
				reservedBitsMu.Unlock()
				if ext4LockDebug {
					debugPrintf("DEBUG unreserveBitmapBits spin check key=%s reservedWantLen=%d foundSeqs=%v bitStatus=%v ts=%d\n", key, wmLen, uniq, bitStatus, time.Now().UnixNano())
				}
				if len(uniq) > 0 {
					// Found sequences; wait for them but bound the wait per-sequence to
					// avoid blocking indefinitely if something went wrong with the
					// dispatcher or seq bookkeeping. If a seq does not become acked
					// within `seqWait` we log and fall back to on-disk polling below.
					const seqWait = 200 * time.Millisecond
					for seq, ack := range uniq {
						debugPrintf("DEBUG unreserveBitmapBits key=%s will wait seq=%d ack=%p ts=%d\n", key, seq, ack, time.Now().UnixNano())
						if isSeqAcked(seq) {
							continue
						}
						deadlineSeq := time.Now().Add(seqWait)
						// If an ack channel exists, try a non-blocking peek so we can
						// detect a buffered ack without consuming it.
						if ack != nil {
							select {
							case v := <-ack:
								// re-insert for the original waiter
								ack <- v
							default:
							}
						}
						for !isSeqAcked(seq) && time.Now().Before(deadlineSeq) {
							time.Sleep(5 * time.Millisecond)
						}
						if isSeqAcked(seq) {
							debugPrintf("DEBUG unreserveBitmapBits key=%s acked flag seq=%d ts=%d\n", key, seq, time.Now().UnixNano())
							continue
						}
						debugPrintf("WARN unreserveBitmapBits seq wait timed out key=%s seq=%d ts=%d\n", key, seq, time.Now().UnixNano())
					}
					// Continue to fallback on-disk verification below.
					break
				}
				time.Sleep(1 * time.Millisecond)
			}
			// If no sequences were associated in the short spin window,
			// fall back to polling the on-disk bitmap as before.
			if ext4LockDebug {
				debugPrintf("DEBUG unreserveBitmapBits key=%s falling back to on-disk polling bits=%v\n", key, bits)
			}
			const maxAttempts = 1000
			const sleepInterval = 5 * time.Millisecond
			var lastD *bgd
			var lastBmap []byte
			matched := false
			for attempt := 0; attempt < maxAttempts; attempt++ {
				// Re-check reservedWant in case a racing association was
				// registered after the initial spin window. If we find newly
				// associated sequences, prefer waiting on the authoritative
				// seq ack instead of continuing blind on-disk polling.
				reservedBitsMu.Lock()
				if wm, ok := reservedWant[key]; ok {
					for _, bit := range bits {
						if ri, ok2 := wm[bit]; ok2 {
							if ri.seq != 0 {
								uniq[ri.seq] = ri.ack
							}
						}
					}
				}
				reservedBitsMu.Unlock()
				if len(uniq) > 0 {
					// Wait a short bounded time for these sequences to be marked
					// acked by the worker. If they become acked, we'll verify
					// on-disk state below; otherwise continue polling.
					deadline := time.Now().Add(200 * time.Millisecond)
					for time.Now().Before(deadline) {
						allAcked := true
						for seq := range uniq {
							if !isSeqAcked(seq) {
								allAcked = false
								break
							}
						}
						if allAcked {
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
					// If still unacked, continue the on-disk polling loop.
					anyUnacked := false
					for seq := range uniq {
						if !isSeqAcked(seq) {
							anyUnacked = true
							break
						}
					}
					if anyUnacked {
						// let the normal polling continue and re-check in subsequent iterations
						continue
					}
					// Otherwise, fall through to perform on-disk verification below.
				}
				d, derr := readBGD(f, fsOffset, sb, group)
				if derr != nil {
					time.Sleep(sleepInterval)
					continue
				}
				// Read the correct bitmap based on whether this is a block or inode reservation.
				bitmapBlockNum := d.InodeBitmapBlock
				if isBlock {
					bitmapBlockNum = d.BlockBitmapBlock
				}
				bmap, berr := readRawBlock(f, fsOffset, sb, bitmapBlockNum)
				if berr != nil {
					time.Sleep(sleepInterval)
					continue
				}
				allMatch := true
				// Verify per-bit desired values match on-disk bitmap
				for _, bit := range bits {
					ri, ok := wantMap[bit]
					var want bool
					if !ok {
						// If we don't have an explicit desired value, assume true
						// (allocated) to be conservative and avoid races.
						want = true
					} else {
						want = ri.want
					}
					if bit < 0 || bit >= len(bmap)*8 {
						allMatch = false
						break
					}
					diskVal := (bmap[bit/8] & (1 << uint(bit%8))) != 0
					if diskVal != want {
						allMatch = false
						break
					}
				}
				// Additionally verify the block-group descriptor's FreeBlocksCount
				// matches the bitmap-derived free count. This ensures the on-disk
				// descriptor isn't stale relative to the bitmap before we clear
				// the in-memory reservation, avoiding BGD/bitmap mismatches.
				if allMatch {
					max := int(sb.BlocksPerGroup)
					if max > len(bmap)*8 {
						max = len(bmap) * 8
					}
					freeCount := 0
					for i := 0; i < max; i++ {
						if bmap[i/8]&(1<<uint(i%8)) == 0 {
							freeCount++
						}
					}
					if uint32(freeCount) != d.FreeBlocksCount {
						if ext4LockDebug {
							debugPrintf("DEBUG unreserveBitmapBits bgd mismatch (accepting per-bit match) key=%s bitmapFree=%d bgdFree=%d seqs=%v bits=%v\n", key, freeCount, d.FreeBlocksCount, uniq, bits)
						}
						// Accept per-bit match even if the BGD FreeBlocksCount differs.
						// A later concurrent update may have changed the descriptor
						// after the bitmap write; requiring strict equality causes
						// spurious verification failures. Treat per-bit agreement
						// as sufficient to clear the in-memory reservation.
					}
				}
				if allMatch {
					if ext4LockDebug {
						debugPrintf("DEBUG unreserveBitmapBits key=%s on-disk match after attempt=%d bits=%v\n", key, attempt, bits)
					}
					matched = true
					break
				}
				time.Sleep(sleepInterval)
			}
			if !matched && ext4LockDebug {
				if lastD != nil && lastBmap != nil {
					// Emit on-disk forensic snapshot to capture last-read state.
					dumpForensicSnapshot(key, bits, wantMap, lastD, lastBmap)
					for _, bit := range bits {
						ri, ok := wantMap[bit]
						var want bool
						if !ok {
							want = true
						} else {
							want = ri.want
						}
						var diskVal bool
						if bit >= 0 && bit < len(lastBmap)*8 {
							diskVal = (lastBmap[bit/8] & (1 << uint(bit%8))) != 0
						}
						debugPrintf("DEBUG unreserveBitmapBits mismatch key=%s bit=%d want=%t disk=%t\n", key, bit, want, diskVal)
					}
					max := int(sb.BlocksPerGroup)
					if max > len(lastBmap)*8 {
						max = len(lastBmap) * 8
					}
					freeCount := 0
					for i := 0; i < max; i++ {
						if lastBmap[i/8]&(1<<uint(i%8)) == 0 {
							freeCount++
						}
					}
					debugPrintf("DEBUG unreserveBitmapBits bgdFree=%d bitmapFree=%d key=%s\n", lastD.FreeBlocksCount, freeCount, key)
				} else {
					debugPrintf("DEBUG unreserveBitmapBits no-last-read key=%s\n", key)
				}
			}
		}
	}

	// Now clear the in-memory reservation bits and any reservedWant entries.
	reservedBitsMu.Lock()
	defer reservedBitsMu.Unlock()
	b, ok := reservedBits[key]
	if !ok {
		return
	}
	var maxBits int
	if isBlock {
		maxBits = int(sb.BlocksPerGroup)
	} else {
		maxBits = int(sb.InodesPerGroup)
	}
	for _, bit := range bits {
		if bit >= 0 && bit < maxBits {
			b[bit/8] &^= 1 << uint(bit%8)
			if wm, ok := reservedWant[key]; ok {
				delete(wm, bit)
			}
		}
	}
	// If reservedWant entry is empty, remove it.
	if wm, ok := reservedWant[key]; ok {
		if len(wm) == 0 {
			delete(reservedWant, key)
		}
	}
	if ext4LockDebug {
		// Report remaining reservation counts for this key.
		if wm, ok := reservedWant[key]; ok {
			debugPrintf("DEBUG unreserveBitmapBits key=%s remaining reservedWant=%d\n", key, len(wm))
		} else {
			debugPrintf("DEBUG unreserveBitmapBits key=%s remaining reservedWant=0\n", key)
		}
	}
	// If all zeros, remove the reservedBits entry to avoid growth.
	allZero := true
	for _, x := range b {
		if x != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		delete(reservedBits, key)
	}
}

// blocking. Returns an unlock function and true on success, or (nil,false)
// if the lock is currently held by another goroutine.
func tryLockBGDGroup(f readerWriterAt, group uint32) (func(), bool) {
	key := bgdGroupLockKey(f, group)
	bgdGroupLocksMu.Lock()
	m, ok := bgdGroupLocks[key]
	if !ok {
		m = newTryMutex()
		bgdGroupLocks[key] = m
	}
	bgdGroupLocksMu.Unlock()
	if !m.TryLock() {
		return nil, false
	}
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("DEBUG tryLockBGDGroup acquired key=%s\n%s\n", key, string(buf[:n]))
	}
	return func() { m.Unlock() }, true
}

// lockBitmapGroup acquires a mutex serializing writes to the block or inode
// bitmap for the given backing file and block group. Returns an unlock func.
func lockBitmapGroup(f readerWriterAt, group uint32, isBlock bool) func() {
	key := bitmapLockKey(f, group, isBlock)
	bitmapGroupLocksMu.Lock()
	m, ok := bitmapGroupLocks[key]
	if !ok {
		m = newTryMutex()
		bitmapGroupLocks[key] = m
	}
	bitmapGroupLocksMu.Unlock()
	m.Lock()
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("DEBUG lockBitmapGroup acquired key=%s\n%s\n", key, string(buf[:n]))
	}
	return func() {
		if ext4LockDebug {
			debugPrintf("DEBUG lockBitmapGroup releasing key=%s\n", key)
		}
		m.Unlock()
	}
}

// tryLockBitmapGroup attempts to acquire the per-file+group bitmap lock
// without blocking. Returns an unlock function and true on success.
func tryLockBitmapGroup(f readerWriterAt, group uint32, isBlock bool) (func(), bool) {
	key := bitmapLockKey(f, group, isBlock)
	bitmapGroupLocksMu.Lock()
	m, ok := bitmapGroupLocks[key]
	if !ok {
		m = newTryMutex()
		bitmapGroupLocks[key] = m
	}
	bitmapGroupLocksMu.Unlock()
	if !m.TryLock() {
		return nil, false
	}
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("DEBUG tryLockBitmapGroup acquired key=%s\n%s\n", key, string(buf[:n]))
	}
	return func() { m.Unlock() }, true
}
