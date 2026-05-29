package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// commitDispatcher serializes block writes for a given backing file so
// callers can release metadata locks before performing potentially slow IO.
// It preserves original blocking semantics by waiting for the worker to
// complete the write (ack channel) but the worker itself performs the IO
// so the caller does not hold metadata locks during write.

type commitTask struct {
	f        readerWriterAt
	fsOffset int64
	sb       *superblock
	group    uint32
	ops      []commitOp
	ack      chan error
	seq      uint64
	// direct indicates the worker should apply writes directly via WriteAt
	// (used for applying journal transactions) rather than calling
	// applyOrJournalWrite. When direct==true, t.sb must be non-nil so the
	// worker can compute block numbers and take inode-table locks where
	// appropriate.
	direct bool
}

type commitWorker struct {
	tasks    chan *commitTask
	key      string
	inWorker int32
	owner    uint64
}

type commitOp struct {
	startAbs int64
	data     []byte
}

var commitWorkersMu sync.Mutex
var commitWorkers = map[string]*commitWorker{}

// commitTaskSeq generates monotonically increasing sequence numbers for
// dispatched commit tasks. These are used to associate in-memory
// reservations with the authoritative apply acknowledgement for a given
// dispatcher task.
var commitTaskSeq uint64

var ackToSeqMu sync.Mutex
var ackToSeq = map[chan error]uint64{}

// ackToSeqTs records the UnixNano timestamp when ack->seq was registered.
// Helps diagnose races where the mapping is deleted too quickly.
var ackToSeqTs = map[chan error]int64{}

var ackedSeqsMu sync.Mutex
var ackedSeqs = map[uint64]bool{}

// seqInfo records basic identifying info for an enqueued commit sequence so
// reservation watchers can correlate newly-created reservations with the
// authoritative sequence for their backing group.
type seqInfo struct {
	f        readerWriterAt
	fsOffset int64
	sb       *superblock
	group    uint32
}

var seqInfosMu sync.Mutex
var seqInfos = map[uint64]seqInfo{}

// seqClaimed records whether a seq from seqInfos has been claimed by an
// enqueued commit task. Protected by seqInfosMu.
var seqClaimed = map[uint64]bool{}

// reservePreassignSeqEnabled controls whether reservation-time pre-assigning
// of dispatcher sequences is enabled. Controlled via EXT4_RESERVE_PREASSIGN_SEQ=1
var reservePreassignSeqEnabled bool

// claimOrAllocateSeq returns a sequence number to use for a new or queued
// commit. It will prefer to claim any previously pre-assigned sequence for
// the same backing group, otherwise it allocates a new sequence via the
// global counter and records seqInfo for watchers.
func claimOrAllocateSeq(f readerWriterAt, fsOffset int64, sb *superblock, group uint32) uint64 {
	// First, try to claim an existing unclaimed seq for this backing group.
	seqInfosMu.Lock()
	var candidate uint64
	for s, si := range seqInfos {
		if si.f == f && si.group == group {
			if !seqClaimed[s] {
				if candidate == 0 || s < candidate {
					candidate = s
				}
			}
		}
	}
	if candidate != 0 {
		seqClaimed[candidate] = true
		seqInfosMu.Unlock()
		return candidate
	}
	seqInfosMu.Unlock()

	// No preassigned seq: allocate a new one and record seqInfo (claimed).
	s := atomic.AddUint64(&commitTaskSeq, 1)
	seqInfosMu.Lock()
	seqInfos[s] = seqInfo{f: f, fsOffset: fsOffset, sb: sb, group: group}
	seqClaimed[s] = true
	seqInfosMu.Unlock()
	return s
}

func registerAckSeq(ack chan error, seq uint64) {
	if ack == nil {
		return
	}
	now := time.Now().UnixNano()
	debugPrintf("DEBUG registerAckSeq ack=%p seq=%d ts=%d\n", ack, seq, now)
	ackToSeqMu.Lock()
	ackToSeq[ack] = seq
	ackToSeqTs[ack] = now
	ackToSeqMu.Unlock()
	// Immediate instrumentation write to help trace races.
	appendAssocLog(fmt.Sprintf("registerAckSeq ack=%p seq=%d ts=%d", ack, seq, now))
}

// attachSeqToReservations attempts to associate the given sequence with any
// in-memory reservations for the specified file/group. It uses the provided
// ops to identify whether the bitmap or inode-bitmap blocks are part of the
// upcoming commit and only associates the seq for those reservation types.
func attachSeqToReservations(f readerWriterAt, fsOffset int64, sb *superblock, group uint32, seq uint64, ops []commitOp) {
	if seq == 0 || f == nil || sb == nil {
		return
	}
	// Read the block-group descriptor to determine bitmap block numbers.
	d, derr := readBGD(f, fsOffset, sb, group)
	if derr != nil {
		return
	}
	bs := int64(sb.BlockSize)
	bmpAbs := fsOffset + int64(d.BlockBitmapBlock)*bs
	ibmpAbs := fsOffset + int64(d.InodeBitmapBlock)*bs

	// Determine which bitmap types are present in ops.
	wantBlock := false
	wantInode := false
	for _, op := range ops {
		if op.startAbs == bmpAbs {
			wantBlock = true
		}
		if op.startAbs == ibmpAbs {
			wantInode = true
		}
	}

	// If neither matched, conservatively associate for both if reservations exist.
	reservedBitsMu.Lock()
	if !wantBlock {
		// check if any block reservations exist for this key
		if wm, ok := reservedWant[bitmapLockKey(f, group, true)]; ok && len(wm) > 0 {
			wantBlock = true
		}
	}
	if !wantInode {
		if wm, ok := reservedWant[bitmapLockKey(f, group, false)]; ok && len(wm) > 0 {
			wantInode = true
		}
	}

	if wantBlock {
		key := bitmapLockKey(f, group, true)
		if wm, ok := reservedWant[key]; ok {
			for bit, ri := range wm {
				if ri.seq == 0 {
					ri.seq = seq
					wm[bit] = ri
				}
			}
			reservedWant[key] = wm
			debugPrintf("DEBUG attachSeqToReservations associated seq=%d key=%s entries=%d ts=%d\n", seq, key, len(wm), time.Now().UnixNano())
		}
	}
	if wantInode {
		key := bitmapLockKey(f, group, false)
		if wm, ok := reservedWant[key]; ok {
			for bit, ri := range wm {
				if ri.seq == 0 {
					ri.seq = seq
					wm[bit] = ri
				}
			}
			reservedWant[key] = wm
			debugPrintf("DEBUG attachSeqToReservations associated seq=%d key=%s entries=%d ts=%d\n", seq, key, len(wm), time.Now().UnixNano())
		}
	}
	reservedBitsMu.Unlock()
}

func getSeqForAck(ack chan error) (uint64, bool) {
	if ack == nil {
		return 0, false
	}
	ackToSeqMu.Lock()
	seq, ok := ackToSeq[ack]
	ts := ackToSeqTs[ack]
	ackToSeqMu.Unlock()
	if ext4LockDebug {
		var age int64
		if ts != 0 {
			age = time.Now().UnixNano() - ts
		}
		debugPrintf("DEBUG getSeqForAck ack=%p seq=%d ok=%t regAgeNs=%d ts=%d\n", ack, seq, ok, age, time.Now().UnixNano())
	}
	return seq, ok
}

func markSeqAcked(seq uint64) {
	if seq == 0 {
		return
	}
	debugPrintf("DEBUG markSeqAcked seq=%d ts=%d\n", seq, time.Now().UnixNano())
	ackedSeqsMu.Lock()
	ackedSeqs[seq] = true
	ackedSeqsMu.Unlock()
}

func isSeqAcked(seq uint64) bool {
	if seq == 0 {
		return false
	}
	ackedSeqsMu.Lock()
	ok := ackedSeqs[seq]
	ackedSeqsMu.Unlock()
	return ok
}

// scheduleAckToSeqDelete removes the ack->seq mapping either immediately
// or after a short asynchronous TTL (configured via
// EXT4_ACK_MAP_GRACE_MS). This avoids blocking the worker while still
// retaining the mapping for a short period to help racing associations
// find authoritative seqs.
func scheduleAckToSeqDelete(ack chan error, seq uint64) {
	if ack == nil {
		return
	}
	if ackToSeqDeleteGrace <= 0 {
		ackToSeqMu.Lock()
		regTs := ackToSeqTs[ack]
		delete(ackToSeq, ack)
		delete(ackToSeqTs, ack)
		ackToSeqMu.Unlock()

		// Also remove any seq->info entry for this sequence to avoid leaks.
		seqInfosMu.Lock()
		delete(seqInfos, seq)
		delete(seqClaimed, seq)
		seqInfosMu.Unlock()
		if ext4LockDebug {
			var age int64
			if regTs != 0 {
				age = time.Now().UnixNano() - regTs
			}
			debugPrintf("DEBUG ackToSeq immediate deleted ack=%p seq=%d regAgeNs=%d ts=%d\n", ack, seq, age, time.Now().UnixNano())
		}
		appendAssocLog(fmt.Sprintf("ackToSeq immediate deleted ack=%p seq=%d regTs=%d ts=%d", ack, seq, regTs, time.Now().UnixNano()))
		return
	}
	// Schedule an asynchronous deletion so the worker goroutine isn't
	// blocked. This retains the mapping for the configured grace window.
	go func(a chan error, s uint64) {
		// initial grace wait
		time.Sleep(ackToSeqDeleteGrace)
		// start time for bounded wait
		start := time.Now()
		// Attempt to delete, but if any reservation still references this
		// ack channel, delay deletion to avoid losing a racing association.
		for {
			// Check whether reservations still reference this ack
			reservedBitsMu.Lock()
			stillReferenced := false
			for _, wm := range reservedWant {
				for _, ri := range wm {
					// Consider a reservation referencing either the ack channel
					// or the authoritative sequence. If either matches, treat
					// the mapping as still referenced so we don't delete it
					// while a racing association is in progress.
					if ri.ack == a || ri.seq == s {
						stillReferenced = true
						break
					}
				}
				if stillReferenced {
					break
				}
			}
			reservedBitsMu.Unlock()

			ackToSeqMu.Lock()
			regTs := ackToSeqTs[a]
			if ext4LockDebug {
				var age int64
				if regTs != 0 {
					age = time.Now().UnixNano() - regTs
				}
				debugPrintf("DEBUG ackToSeq ttl deleting ack=%p seq=%d regAgeNs=%d grace=%s referenced=%t ts=%d\n", a, s, age, ackToSeqDeleteGrace, stillReferenced, time.Now().UnixNano())
			}
			if !stillReferenced {
				delete(ackToSeq, a)
				delete(ackToSeqTs, a)
				ackToSeqMu.Unlock()
				// Also remove seq->info and claimed marker when we finally drop the mapping.
				seqInfosMu.Lock()
				delete(seqInfos, s)
				delete(seqClaimed, s)
				seqInfosMu.Unlock()
				appendAssocLog(fmt.Sprintf("ackToSeq ttl delete ack=%p seq=%d regTs=%d ts=%d", a, s, regTs, time.Now().UnixNano()))
				return
			}
			// If we've exceeded the configured maximum, force deletion to
			// avoid unbounded retention.
			if ackToSeqDeleteMax > 0 && time.Since(start) >= ackToSeqDeleteMax {
				delete(ackToSeq, a)
				delete(ackToSeqTs, a)
				ackToSeqMu.Unlock()
				// forced deletion: also drop seq info and claimed marker
				seqInfosMu.Lock()
				delete(seqInfos, s)
				delete(seqClaimed, s)
				seqInfosMu.Unlock()
				debugPrintf("WARN ackToSeq forced delete ack=%p seq=%d elapsed=%s ts=%d\n", a, s, time.Since(start), time.Now().UnixNano())
				appendAssocLog(fmt.Sprintf("ackToSeq forced delete ack=%p seq=%d elapsed=%s ts=%d", a, s, time.Since(start), time.Now().UnixNano()))
				return
			}
			// release lock and wait a short interval before retrying.
			ackToSeqMu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
	}(ack, seq)
}

// commitWorkerIdle is the duration a worker will stay alive while idle
// before removing itself from the global map and exiting. Can be tuned
// via the EXT4_COMMIT_WORKER_IDLE_SEC environment variable for tests.
var commitWorkerIdle = 10 * time.Second

// ackToSeqDeleteGrace is an optional test-only grace period to retain the
// ack->seq mapping after sending the ack. Set via EXT4_ACK_MAP_GRACE_MS
// (milliseconds) to experiment with races where associations run slightly
// after the worker completes and deletes the mapping.
var ackToSeqDeleteGrace time.Duration

// ackToSeqDeleteMax is a bounded cap on how long the background deleter
// will continue to defer removing the ack->seq mapping while reservations
// still reference the ack channel. Default is 5s; overridden via
// EXT4_ACK_MAP_MAX_MS for tests.
var ackToSeqDeleteMax time.Duration

func init() {
	if v := os.Getenv("EXT4_COMMIT_WORKER_IDLE_SEC"); v != "" {
		if s, err := strconv.Atoi(v); err == nil && s >= 0 {
			commitWorkerIdle = time.Duration(s) * time.Second
		}
	}
	if v := os.Getenv("EXT4_ACK_MAP_GRACE_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			ackToSeqDeleteGrace = time.Duration(ms) * time.Millisecond
			if ext4LockDebug {
				debugPrintf("DEBUG ackToSeqDeleteGrace set to %dms ts=%d\n", ms, time.Now().UnixNano())
			}
		}
	}
	// configure optional maximum wait for ack->seq retention
	ackToSeqDeleteMax = 5 * time.Second
	if v := os.Getenv("EXT4_ACK_MAP_MAX_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			ackToSeqDeleteMax = time.Duration(ms) * time.Millisecond
			if ext4LockDebug {
				debugPrintf("DEBUG ackToSeqDeleteMax set to %dms ts=%d\n", ms, time.Now().UnixNano())
			}
		}
	}

	// Optional behavior: preassign dispatcher sequences at reservation
	// creation time. Enabled by setting EXT4_RESERVE_PREASSIGN_SEQ=1.
	if v := os.Getenv("EXT4_RESERVE_PREASSIGN_SEQ"); v == "1" {
		reservePreassignSeqEnabled = true
		if ext4LockDebug {
			debugPrintf("DEBUG reservePreassignSeqEnabled=true ts=%d\n", time.Now().UnixNano())
		}
	}
}

// appliedDumpPath returns a unique path for writing applied-block
// diagnostics. Include PID and a nanosecond timestamp so multiple
// writes for the same seq/block don't clobber each other.
func appliedDumpPath(seq uint64, blk uint64) string {
	return fmt.Sprintf("/tmp/ext4_seq_%d_block_%d_applied-%d-%d.bin", seq, blk, os.Getpid(), time.Now().UnixNano())
}

func ensureCommitWorker(key string) *commitWorker {
	commitWorkersMu.Lock()
	defer commitWorkersMu.Unlock()
	if w, ok := commitWorkers[key]; ok {
		return w
	}
	w := &commitWorker{tasks: make(chan *commitTask, 1024), key: key}
	commitWorkers[key] = w
	go w.run()
	return w
}

func (w *commitWorker) run() {
	idleTimer := time.NewTimer(commitWorkerIdle)
	defer idleTimer.Stop()
	for {
		select {
		case t, ok := <-w.tasks:
			if !ok {
				return
			}
			// reset idle timer
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(commitWorkerIdle)
			var err error
			// Mark that this goroutine is actively processing tasks for
			// this worker. Record the goroutine id so enqueue helpers can
			// detect when the caller is the same worker and fall back to
			// performing writes inline to avoid self-enqueue deadlocks.
			atomic.StoreUint64(&w.owner, getGID())
			atomic.StoreInt32(&w.inWorker, 1)
			debugPrintf("DEBUG commitWorker starting task seq=%d group=%d direct=%t ops=%d ts=%d\n", t.seq, t.group, t.direct, len(t.ops), time.Now().UnixNano())
			if t.direct {
				debugPrintf("DEBUG commitWorker applying direct seq=%d group=%d ops=%d\n", t.seq, t.group, len(t.ops))
				// Apply writes directly using the centralized helper which
				// performs test tracing and acquires any needed inode-table
				// locks for overlapping groups. To reduce contention we only
				// acquire the per-group BGD lock around actual descriptor
				// writes rather than holding it for the entire task duration.
				// Apply writes directly using the centralized helper which
				// performs test tracing and acquires any needed inode-table
				// locks for overlapping groups.
				var lastBitmap []byte
				var lastBitmapStartAbs int64 = -1
				for idx, op := range t.ops {
					// Log synchronous write intent (addr + len) so we can observe
					// the exact order the worker issues WriteAt calls. Also
					// attempt to detect BGD/bitmap/superblock writes and log
					// the decoded free-counts for BGD descriptors to help
					// correlate mismatches observed in tests.
					if len(op.data) > 0 {
						debugPrintf("DEBUG commitWorker direct write seq=%d group=%d idx=%d startAbs=%d len=%d addr=%p\n", t.seq, t.group, idx, op.startAbs, len(op.data), &op.data[0])
					} else {
						debugPrintf("DEBUG commitWorker direct write seq=%d group=%d idx=%d startAbs=%d len=0 addr=nil\n", t.seq, t.group, idx, op.startAbs)
					}
					if ext4LockDebug && t.sb != nil && len(op.data) > 0 {
						// Heuristics: bitmap writes are block-sized, BGD writes are
						// DescSize, superblock is typically 1024 bytes.
						if len(op.data) == int(t.sb.BlockSize) {
							debugPrintf("DEBUG commitWorker detected bitmap write seq=%d group=%d idx=%d startAbs=%d len=%d\n", t.seq, t.group, idx, op.startAbs, len(op.data))
							// Capture a copy of the bitmap bytes so we can compare
							// bitmap-derived free count with the descriptor's
							// FreeBlocksCount when the BGD write is applied.
							lastBitmap = make([]byte, len(op.data))
							copy(lastBitmap, op.data)
							lastBitmapStartAbs = op.startAbs
							// Optionally dump applied bitmap bytes for diagnostics
							if os.Getenv("EXT4_DUMP_TX_BLOCKS") == "1" {
								bs := int64(t.sb.BlockSize)
								var blk uint64
								if t.fsOffset <= op.startAbs {
									blk = uint64((op.startAbs - t.fsOffset) / bs)
								} else {
									blk = 0
								}
								dumpPath := appliedDumpPath(t.seq, blk)
								_ = os.WriteFile(dumpPath, op.data, 0644)
								debugPrintf("DEBUG commitWorker seq=%d dumped applied block=%d path=%s ts=%d\n", t.seq, blk, dumpPath, time.Now().UnixNano())
							}
						} else if len(op.data) == int(t.sb.DescSize) {
							// Decode FreeBlocksCount from descriptor bytes.
							le := binary.LittleEndian
							var free uint32
							if t.sb.FeatureIncompat&FeatIncompat64bit != 0 && int(t.sb.DescSize) >= 64 {
								free = uint32(le.Uint16(op.data[12:])) | (uint32(le.Uint16(op.data[44:])) << 16)
							} else {
								free = uint32(le.Uint16(op.data[12:]))
							}
							debugPrintf("DEBUG commitWorker detected BGD write seq=%d group=%d idx=%d startAbs=%d freeBlocks=%d len=%d\n", t.seq, t.group, idx, op.startAbs, free, len(op.data))
							if lastBitmap != nil {
								// compute bitmap-derived free count for diagnostics
								max := int(t.sb.BlocksPerGroup)
								if max > len(lastBitmap)*8 {
									max = len(lastBitmap) * 8
								}
								bf := 0
								for i := 0; i < max; i++ {
									if lastBitmap[i/8]&(1<<uint(i%8)) == 0 {
										bf++
									}
								}
								debugPrintf("DEBUG commitWorker bitmapFree=%d bgdFree=%d seq=%d\n", bf, free, t.seq)
							}
						} else if len(op.data) == 1024 {
							debugPrintf("DEBUG commitWorker detected superblock write seq=%d group=%d idx=%d startAbs=%d len=%d\n", t.seq, t.group, idx, op.startAbs, len(op.data))
						}
					}

					// Before performing the actual BGD write, recompute authoritative
					// FreeBlocksCount and FreeInodesCount from the most recent bitmaps
					// when possible and update the descriptor bytes in-place so the
					// on-disk BGD reflects the current bitmap-derived counts.
					if t.sb != nil && len(op.data) == int(t.sb.DescSize) {
						d := decodeBGD(op.data, t.sb)
						var bf int = -1
						var fi int = -1
						bs := int64(t.sb.BlockSize)
						// Compute block bitmap absolute offset and attempt to use
						// the captured in-task bitmap when it matches the same
						// absolute offset; otherwise read from backing file.
						bbAbs := t.fsOffset + int64(d.BlockBitmapBlock)*bs
						if lastBitmap != nil && lastBitmapStartAbs == bbAbs {
							max := int(t.sb.BlocksPerGroup)
							if max > len(lastBitmap)*8 {
								max = len(lastBitmap) * 8
							}
							cnt := 0
							for i := 0; i < max; i++ {
								if lastBitmap[i/8]&(1<<uint(i%8)) == 0 {
									cnt++
								}
							}
							bf = cnt
						} else if t.f != nil {
							bmp := make([]byte, int(t.sb.BlockSize))
							if _, rerr := t.f.ReadAt(bmp, bbAbs); rerr == nil {
								max := int(t.sb.BlocksPerGroup)
								if max > len(bmp)*8 {
									max = len(bmp) * 8
								}
								cnt := 0
								for i := 0; i < max; i++ {
									if bmp[i/8]&(1<<uint(i%8)) == 0 {
										cnt++
									}
								}
								bf = cnt
							} else if ext4LockDebug {
								debugPrintf("DEBUG commitWorker could not read block bitmap for BGD recompute seq=%d group=%d idx=%d err=%v\n", t.seq, t.group, idx, rerr)
							}
						}
						// Compute inode bitmap offset and authoritative inode free count
						ibAbs := t.fsOffset + int64(d.InodeBitmapBlock)*bs
						if lastBitmap != nil && lastBitmapStartAbs == ibAbs {
							max := int(t.sb.InodesPerGroup)
							if max > len(lastBitmap)*8 {
								max = len(lastBitmap) * 8
							}
							cnt := 0
							for i := 0; i < max; i++ {
								if lastBitmap[i/8]&(1<<uint(i%8)) == 0 {
									cnt++
								}
							}
							fi = cnt
						} else if t.f != nil {
							ibmp := make([]byte, int(t.sb.BlockSize))
							if _, rerr := t.f.ReadAt(ibmp, ibAbs); rerr == nil {
								max := int(t.sb.InodesPerGroup)
								if max > len(ibmp)*8 {
									max = len(ibmp) * 8
								}
								cnt := 0
								for i := 0; i < max; i++ {
									if ibmp[i/8]&(1<<uint(i%8)) == 0 {
										cnt++
									}
								}
								fi = cnt
							} else if ext4LockDebug {
								debugPrintf("DEBUG commitWorker could not read inode bitmap for BGD recompute seq=%d group=%d idx=%d err=%v\n", t.seq, t.group, idx, rerr)
							}
						}
						changed := false
						if bf >= 0 && uint32(bf) != d.FreeBlocksCount {
							old := d.FreeBlocksCount
							d.FreeBlocksCount = uint32(bf)
							if ext4LockDebug {
								debugPrintf("DEBUG commitWorker updated BGD FreeBlocksCount from=%d to=%d seq=%d group=%d idx=%d\n", old, d.FreeBlocksCount, t.seq, t.group, idx)
							}
							changed = true
						}
						if fi >= 0 && uint32(fi) != d.FreeInodesCount {
							old := d.FreeInodesCount
							d.FreeInodesCount = uint32(fi)
							if ext4LockDebug {
								debugPrintf("DEBUG commitWorker updated BGD FreeInodesCount from=%d to=%d seq=%d group=%d idx=%d\n", old, d.FreeInodesCount, t.seq, t.group, idx)
							}
							changed = true
						}
						if changed {
							d.encode(t.sb)
							if t.sb.FeatureROCompat&FeatROCompatMetadataCsum != 0 {
								le := binary.LittleEndian
								raw := d.raw
								le.PutUint16(raw[30:], 0)
								gLE := make([]byte, 4)
								le.PutUint32(gLE, uint32(t.group))
								csum := crc32c(t.sb.csumSeed(), gLE)
								csum = crc32c(csum, raw[:t.sb.DescSize])
								le.PutUint16(raw[30:], uint16(csum))
							}

							// Focused forensic snapshot: when a BGD write occurs for a
							// sequence of interest (set via EXT4_FOCUS_SEQ), capture the
							// descriptor and the last-seen bitmap bytes so we have an
							// authoritative on-disk artifact for post-mortem analysis.
							if ext4LockDebug {
								if fsEnv := os.Getenv("EXT4_FOCUS_SEQ"); fsEnv != "" {
									if fsVal, err := strconv.ParseUint(fsEnv, 10, 64); err == nil {
										if fsVal == t.seq {
											key := bitmapLockKey(t.f, t.group, true)
											// run async to avoid blocking the worker
											go dumpForensicSnapshot(key, nil, nil, d, lastBitmap)
											debugPrintf("DEBUG commitWorker forensicDump seq=%d key=%s ts=%d\n", t.seq, key, time.Now().UnixNano())
										}
									}
								}
							}
						}
					}
					var werr error
					if t.sb != nil && len(op.data) == int(t.sb.DescSize) && t.f != nil {
						// Descriptor write: briefly acquire per-group BGD lock
						// while performing the actual write to serialize
						// descriptor updates with other metadata ops.
						unlockBGDLocal := lockBGDGroup(t.f, t.group)
						if _, we := writeAtWithTrace(t.f, t.fsOffset, t.sb, op.startAbs, op.data); we != nil {
							werr = we
						}
						if unlockBGDLocal != nil {
							unlockBGDLocal()
						}
					} else {
						if _, we := writeAtWithTrace(t.f, t.fsOffset, t.sb, op.startAbs, op.data); we != nil {
							werr = we
						}
					}
					if werr != nil {
						debugPrintf("WARN commitWorker direct write failed: group=%d idx=%d off=%d len=%d err=%v\n", t.group, idx, op.startAbs, len(op.data), werr)
						var buf [4096]byte
						n := runtime.Stack(buf[:], false)
						debugPrintf("WARN commitWorker stacks:\n%s\n", string(buf[:n]))
						err = werr
						break
					}

					// Optional read-back of applied bytes for diagnostics. This ensures
					// we capture what actually landed on disk even when earlier
					// heuristics didn't create an applied dump.
					if os.Getenv("EXT4_DUMP_TX_BLOCKS") == "1" && t.f != nil && t.sb != nil {
						bs := int64(t.sb.BlockSize)
						var blk uint64
						if t.fsOffset <= op.startAbs {
							blk = uint64((op.startAbs - t.fsOffset) / bs)
						} else {
							blk = 0
						}
						buf := make([]byte, bs)
						if _, rerr := t.f.ReadAt(buf, op.startAbs); rerr == nil {
							dumpPath := appliedDumpPath(t.seq, blk)
							_ = os.WriteFile(dumpPath, buf, 0644)
							debugPrintf("DEBUG commitWorker seq=%d dumped applied block(readback)=%d path=%s ts=%d\n", t.seq, blk, dumpPath, time.Now().UnixNano())
						} else if ext4LockDebug {
							debugPrintf("DEBUG commitWorker readback failed seq=%d blk=%d err=%v\n", t.seq, blk, rerr)
						}
					}
					// Log completion so we observe actual write completions (post-IO).
					if len(op.data) > 0 {
						debugPrintf("DEBUG commitWorker completed write seq=%d group=%d idx=%d startAbs=%d len=%d addr=%p\n", t.seq, t.group, idx, op.startAbs, len(op.data), &op.data[0])
					} else {
						debugPrintf("DEBUG commitWorker completed write seq=%d group=%d idx=%d startAbs=%d len=0 addr=nil\n", t.seq, t.group, idx, op.startAbs)
					}
				}
			} else {
				debugPrintf("DEBUG commitWorker applying normal group=%d ops=%d\n", t.group, len(t.ops))
				for _, op := range t.ops {
					err = applyOrJournalWrite(t.f, t.fsOffset, t.sb, op.startAbs, op.data)
					if err != nil {
						break
					}

					// Read-back applied bytes for diagnostics when requested. This
					// captures what was actually written to the backing file.
					if os.Getenv("EXT4_DUMP_TX_BLOCKS") == "1" && t.f != nil && t.sb != nil {
						bs := int64(t.sb.BlockSize)
						var blk uint64
						if t.fsOffset <= op.startAbs {
							blk = uint64((op.startAbs - t.fsOffset) / bs)
						} else {
							blk = 0
						}
						buf := make([]byte, bs)
						if _, rerr := t.f.ReadAt(buf, op.startAbs); rerr == nil {
							dumpPath := appliedDumpPath(t.seq, blk)
							_ = os.WriteFile(dumpPath, buf, 0644)
							debugPrintf("DEBUG commitWorker seq=%d dumped applied block(readback)=%d path=%s ts=%d\n", t.seq, blk, dumpPath, time.Now().UnixNano())
						} else if ext4LockDebug {
							debugPrintf("DEBUG commitWorker readback failed seq=%d blk=%d err=%v\n", t.seq, blk, rerr)
						}
					}
				}
			}
			if t.ack != nil {
				debugPrintf("DEBUG commitWorker sending ack seq=%d group=%d ack=%p err=%v ts=%d\n", t.seq, t.group, t.ack, err, time.Now().UnixNano())
				t.ack <- err
				debugPrintf("DEBUG commitWorker sent ack seq=%d group=%d ack=%p err=%v ts=%d\n", t.seq, t.group, t.ack, err, time.Now().UnixNano())
				// Mark this sequence as acked so callers waiting on the logical
				// reservation can detect completion even if they've already
				// consumed the returned ack channel value.
				if t.seq != 0 {
					markSeqAcked(t.seq)
					debugPrintf("DEBUG commitWorker markSeqAcked seq=%d ts=%d\n", t.seq, time.Now().UnixNano())
				}
				// Remove any ack->seq mapping to avoid growth. For diagnostics
				// we can optionally retain the mapping for a short grace period
				// (controlled by EXT4_ACK_MAP_GRACE_MS) to observe racing
				// associations that query the mapping slightly later.
				if ext4LockDebug {
					debugPrintf("DEBUG commitWorker scheduling ackToSeq delete ack=%p seq=%d grace=%s ts=%d\n", t.ack, t.seq, ackToSeqDeleteGrace, time.Now().UnixNano())
				}
				// Use the TTL-based scheduler to remove the mapping without
				// blocking the worker. The scheduler will delete immediately
				// when TTL==0.
				scheduleAckToSeqDelete(t.ack, t.seq)
			}
			// Clear active processing flag and owner after acknowledgement
			// so that other callers know the worker is available again.
			atomic.StoreUint64(&w.owner, 0)
			atomic.StoreInt32(&w.inWorker, 0)
		case <-idleTimer.C:
			// Attempt to remove ourselves from the global map and exit if
			// we're still the registered worker for this key. If we've been
			// replaced, just continue servicing tasks on this channel.
			commitWorkersMu.Lock()
			if commitWorkers[w.key] == w {
				delete(commitWorkers, w.key)
				commitWorkersMu.Unlock()
				return
			}
			commitWorkersMu.Unlock()
			// someone else registered a worker for this key; reset timer
			idleTimer.Reset(commitWorkerIdle)
		}
	}
}

// enqueueCommitWritesUnderLock enqueues a batch of commits while the caller
// still holds any per-group locks. It returns an ack channel that the caller
// should wait on after releasing those locks. This preserves ordering (the
// task is queued while the caller holds the BGD lock) but avoids blocking
// the caller while holding that lock.
func enqueueCommitWritesUnderLock(f readerWriterAt, fsOffset int64, sb *superblock, group uint32, ops []commitOp) (chan error, uint64, error) {
	if f == nil {
		ack := make(chan error, 1)
		seq := claimOrAllocateSeq(f, fsOffset, sb, group)
		registerAckSeq(ack, seq)
		// Try to eagerly attach seq to any existing reservations for this
		// backing group.
		attachSeqToReservations(f, fsOffset, sb, group, seq, ops)
		// Optionally dump prepared op data for diagnostics when no worker is used.
		if os.Getenv("EXT4_DUMP_TX_BLOCKS") == "1" && sb != nil {
			bs := int64(sb.BlockSize)
			for _, op := range ops {
				if len(op.data) == int(bs) {
					var blk uint64
					if fsOffset <= op.startAbs {
						blk = uint64((op.startAbs - fsOffset) / bs)
					}
					dumpPath := fmt.Sprintf("/tmp/ext4_seq_%d_block_%d_prepared.bin", seq, blk)
					_ = os.WriteFile(dumpPath, op.data, 0644)
					debugPrintf("DEBUG enqueueCommitWrites f==nil seq=%d dumped prepared block=%d path=%s ts=%d\n", seq, blk, dumpPath, time.Now().UnixNano())
				}
			}
		}
		go func(seq uint64, ack chan error) {
			var lastErr error
			for _, op := range ops {
				if err := applyOrJournalWrite(f, fsOffset, sb, op.startAbs, op.data); err != nil {
					lastErr = err
					break
				}
			}
			ack <- lastErr
			// mark as acked and cleanup mapping
			markSeqAcked(seq)
			// schedule deletion (immediate if TTL==0)
			scheduleAckToSeqDelete(ack, seq)
		}(seq, ack)
		return ack, seq, nil
	}
	key := fmt.Sprintf("%s:g:%d", fileLockKey(f), group)
	w := ensureCommitWorker(key)
	if atomic.LoadUint64(&w.owner) == getGID() {
		debugPrintf("DEBUG enqueueCommitWrites inline-inworker fallback group=%d ops=%d\n", group, len(ops))
		ack := make(chan error, 1)
		seq := claimOrAllocateSeq(f, fsOffset, sb, group)
		registerAckSeq(ack, seq)
		// Record seq->info already done by claimOrAllocateSeq; eagerly associate
		// while still holding caller locks to avoid racing deletions.
		attachSeqToReservations(f, fsOffset, sb, group, seq, ops)
		if os.Getenv("EXT4_DUMP_TX_BLOCKS") == "1" && sb != nil {
			bs := int64(sb.BlockSize)
			for _, op := range ops {
				if len(op.data) == int(bs) {
					var blk uint64
					if fsOffset <= op.startAbs {
						blk = uint64((op.startAbs - fsOffset) / bs)
					}
					dumpPath := fmt.Sprintf("/tmp/ext4_seq_%d_block_%d_prepared.bin", seq, blk)
					_ = os.WriteFile(dumpPath, op.data, 0644)
					debugPrintf("DEBUG enqueueCommitWrites inline seq=%d dumped prepared block=%d path=%s ts=%d\n", seq, blk, dumpPath, time.Now().UnixNano())
				}
			}
		}
		var lastErr error
		for _, op := range ops {
			if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
				lastErr = err
				break
			}
		}
		ack <- lastErr
		// inline path: mark seq acked and cleanup mapping
		markSeqAcked(seq)
		scheduleAckToSeqDelete(ack, seq)
		return ack, seq, nil
	}
	// Diagnostic: log the backing-array address and length for each op's
	// data to catch cases where a live slice is passed to the dispatcher.
	for i, op := range ops {
		if len(op.data) > 0 {
			debugPrintf("DEBUG enqueueCommitWrites group=%d op=%d addr=%p len=%d startAbs=%d\n", group, i, &op.data[0], len(op.data), op.startAbs)
		} else {
			debugPrintf("DEBUG enqueueCommitWrites group=%d op=%d addr=nil len=0 startAbs=%d\n", group, i, op.startAbs)
		}
	}
	debugPrintf("DEBUG enqueueCommitWrites group=%d ops=%d\n", group, len(ops))
	// Enqueue task as direct writes so the worker will apply the ops via
	// `writeAtWithTrace` rather than creating per-op transactions. This
	// avoids isolated journal commits originating from allocator helpers
	// and preserves ordering as the caller holds group locks while
	// enqueueing.
	t := &commitTask{f: f, fsOffset: fsOffset, sb: sb, group: group, ops: ops, ack: make(chan error, 1), direct: true}
	// assign a sequence for this queued task and register mapping (try claim)
	t.seq = claimOrAllocateSeq(f, fsOffset, sb, group)
	registerAckSeq(t.ack, t.seq)
	// attach sequence with reservations for this group while still under the
	// caller's BGD lock so racing associations see it.
	attachSeqToReservations(f, fsOffset, sb, group, t.seq, ops)
	// Optionally dump prepared op data for diagnostics before enqueue.
	if os.Getenv("EXT4_DUMP_TX_BLOCKS") == "1" && sb != nil {
		bs := int64(sb.BlockSize)
		for _, op := range ops {
			if len(op.data) == int(bs) {
				var blk uint64
				if fsOffset <= op.startAbs {
					blk = uint64((op.startAbs - fsOffset) / bs)
				}
				dumpPath := fmt.Sprintf("/tmp/ext4_seq_%d_block_%d_prepared.bin", t.seq, blk)
				_ = os.WriteFile(dumpPath, op.data, 0644)
				debugPrintf("DEBUG enqueueCommitWrites queued seq=%d dumped prepared block=%d path=%s ts=%d\n", t.seq, blk, dumpPath, time.Now().UnixNano())
			}
		}
	}
	select {
	case w.tasks <- t:
		if ext4LockDebug {
			debugPrintf("DEBUG enqueueCommitWrites enqueued seq=%d ack=%p group=%d ops=%d ts=%d\n", t.seq, t.ack, group, len(ops), time.Now().UnixNano())
		}
		return t.ack, t.seq, nil
	default:
		debugPrintf("DEBUG enqueueCommitWrites queue-full fallback group=%d ops=%d\n", group, len(ops))
		ack := make(chan error, 1)
		seq := claimOrAllocateSeq(f, fsOffset, sb, group)
		registerAckSeq(ack, seq)
		if os.Getenv("EXT4_DUMP_TX_BLOCKS") == "1" && sb != nil {
			bs := int64(sb.BlockSize)
			for _, op := range ops {
				if len(op.data) == int(bs) {
					var blk uint64
					if fsOffset <= op.startAbs {
						blk = uint64((op.startAbs - fsOffset) / bs)
					}
					dumpPath := fmt.Sprintf("/tmp/ext4_seq_%d_block_%d_prepared.bin", seq, blk)
					_ = os.WriteFile(dumpPath, op.data, 0644)
					debugPrintf("DEBUG enqueueCommitWrites fallback seq=%d dumped prepared block=%d path=%s ts=%d\n", seq, blk, dumpPath, time.Now().UnixNano())
				}
			}
		}
		go func(seq uint64, ack chan error) {
			var lastErr error
			for _, op := range ops {
				if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
					lastErr = err
					break
				}
			}
			ack <- lastErr
			markSeqAcked(seq)
			scheduleAckToSeqDelete(ack, seq)
		}(seq, ack)
		return ack, seq, nil
	}
}

// enqueueCommitWritesDirectUnderLock is like enqueueCommitWritesUnderLock
// but instructs the worker to apply writes directly via WriteAt (used for
// applying journal transactions). It returns an ack channel the caller must
// wait on after releasing any held group locks.
func enqueueCommitWritesDirectUnderLock(f readerWriterAt, fsOffset int64, sb *superblock, group uint32, ops []commitOp, seq uint64) (chan error, error) {
	if f == nil {
		ack := make(chan error, 1)
		s := claimOrAllocateSeq(f, fsOffset, sb, group)
		registerAckSeq(ack, s)
		go func(s uint64, ack chan error) {
			var lastErr error
			for _, op := range ops {
				if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
					lastErr = err
					break
				}
			}
			ack <- lastErr
			markSeqAcked(s)
			scheduleAckToSeqDelete(ack, s)
		}(s, ack)
		return ack, nil
	}
	debugPrintf("DEBUG enqueueCommitWritesDirect seq=%d group=%d ops=%d\n", seq, group, len(ops))
	key := fmt.Sprintf("%s:g:%d", fileLockKey(f), group)
	w := ensureCommitWorker(key)
	if atomic.LoadUint64(&w.owner) == getGID() {
		debugPrintf("DEBUG enqueueCommitWritesDirect inline-inworker fallback seq=%d group=%d ops=%d\n", seq, group, len(ops))
		ack := make(chan error, 1)
		if seq == 0 {
			seq = claimOrAllocateSeq(f, fsOffset, sb, group)
		}
		registerAckSeq(ack, seq)
		// record seq->info for reservation watchers
		seqInfosMu.Lock()
		seqInfos[seq] = seqInfo{f: f, fsOffset: fsOffset, sb: sb, group: group}
		seqInfosMu.Unlock()
		var lastErr error
		for _, op := range ops {
			if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
				lastErr = err
				break
			}
		}
		ack <- lastErr
		markSeqAcked(seq)
		scheduleAckToSeqDelete(ack, seq)
		return ack, nil
	}
	if seq == 0 {
		seq = claimOrAllocateSeq(f, fsOffset, sb, group)
	}
	t := &commitTask{f: f, fsOffset: fsOffset, sb: sb, group: group, ops: ops, ack: make(chan error, 1), direct: true, seq: seq}
	registerAckSeq(t.ack, t.seq)
	// record seq->info for reservation watchers
	seqInfosMu.Lock()
	seqInfos[t.seq] = seqInfo{f: f, fsOffset: fsOffset, sb: sb, group: group}
	seqInfosMu.Unlock()
	select {
	case w.tasks <- t:
		return t.ack, nil
	default:
		debugPrintf("DEBUG enqueueCommitWritesDirect queue-full fallback seq=%d group=%d ops=%d\n", seq, group, len(ops))
		ack := make(chan error, 1)
		registerAckSeq(ack, seq)
		// record seq->info for reservation watchers
		seqInfosMu.Lock()
		seqInfos[seq] = seqInfo{f: f, fsOffset: fsOffset, sb: sb, group: group}
		seqInfosMu.Unlock()
		go func(seq uint64, ack chan error) {
			var lastErr error
			for _, op := range ops {
				if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
					lastErr = err
					break
				}
			}
			ack <- lastErr
			markSeqAcked(seq)
			scheduleAckToSeqDelete(ack, seq)
		}(seq, ack)
		return ack, nil
	}
}

// enqueueCommitWrite enqueues a write task for the backing file identified
// by `f`. If the dispatch queue is full, it falls back to performing the
// write synchronously in the caller.
// enqueueCommitWrites enqueues a batch of writes for the given backing
// file and block-group as a single task. This ensures the batch is applied
// atomically (in-order) relative to other batches for the same group.
func enqueueCommitWrites(f readerWriterAt, fsOffset int64, sb *superblock, group uint32, ops []commitOp) error {
	if f == nil {
		// fallback: perform operations inline
		for _, op := range ops {
			if err := applyOrJournalWrite(f, fsOffset, sb, op.startAbs, op.data); err != nil {
				return err
			}
		}
		return nil
	}
	key := fmt.Sprintf("%s:g:%d", fileLockKey(f), group)
	w := ensureCommitWorker(key)
	// If the worker is currently processing tasks (i.e. we're running inside
	// the worker's goroutine), avoid enqueuing to ourselves which would
	// deadlock. Perform writes inline instead.
	if atomic.LoadUint64(&w.owner) == getGID() {
		debugPrintf("DEBUG enqueueCommitWrites inline-inworker fallback group=%d ops=%d\n", group, len(ops))
		for _, op := range ops {
			if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
				return err
			}
		}
		return nil
	}
	debugPrintf("DEBUG enqueueCommitWrites group=%d ops=%d\n", group, len(ops))
	t := &commitTask{f: f, fsOffset: fsOffset, sb: sb, group: group, ops: ops, ack: make(chan error, 1)}
	// Assign a sequence (try to claim any preassigned seq) and register ack->seq mapping
	// so racing reservations can associate the authoritative sequence while the
	// caller still holds metadata locks.
	t.seq = claimOrAllocateSeq(f, fsOffset, sb, group)
	registerAckSeq(t.ack, t.seq)
	// record seq->info for reservation watchers
	seqInfosMu.Lock()
	seqInfos[t.seq] = seqInfo{f: f, fsOffset: fsOffset, sb: sb, group: group}
	seqInfosMu.Unlock()
	select {
	case w.tasks <- t:
		return <-t.ack
	default:
		debugPrintf("DEBUG enqueueCommitWrites queue-full fallback group=%d ops=%d\n", group, len(ops))
		for _, op := range ops {
			if err := applyOrJournalWrite(f, fsOffset, sb, op.startAbs, op.data); err != nil {
				return err
			}
		}
		return nil
	}
}

// enqueueCommitWritesDirect enqueues a commit task whose writes will be
// applied directly by the worker (via WriteAt) rather than using the
// journaling helpers. This is used to apply journal transactions without
// re-journaling them.
func enqueueCommitWritesDirect(f readerWriterAt, fsOffset int64, sb *superblock, group uint32, ops []commitOp) error {
	if f == nil {
		// fallback: perform operations inline (route through centralized
		// helper so test tracing and inode-table locking are used when
		// possible). Note: callers should not pass a nil `f` in normal
		// operation; this preserves previous behavior while centralizing
		// the write path.
		for _, op := range ops {
			if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
				return err
			}
		}
		return nil
	}
	debugPrintf("DEBUG enqueueCommitWritesDirect group=%d ops=%d\n", group, len(ops))
	key := fmt.Sprintf("%s:g:%d", fileLockKey(f), group)
	w := ensureCommitWorker(key)
	// Avoid enqueuing to ourselves; fall back to performing direct writes
	// inline when the worker is actively processing tasks.
	if atomic.LoadUint64(&w.owner) == getGID() {
		debugPrintf("DEBUG enqueueCommitWritesDirect inline-inworker fallback group=%d ops=%d\n", group, len(ops))
		for _, op := range ops {
			if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
				return err
			}
		}
		return nil
	}
	t := &commitTask{f: f, fsOffset: fsOffset, sb: sb, group: group, ops: ops, ack: make(chan error, 1), direct: true}
	select {
	case w.tasks <- t:
		return <-t.ack
	default:
		debugPrintf("DEBUG enqueueCommitWritesDirect queue-full fallback group=%d ops=%d\n", group, len(ops))
		for _, op := range ops {
			if _, err := writeAtWithTrace(f, fsOffset, sb, op.startAbs, op.data); err != nil {
				return err
			}
		}
		return nil
	}
}

// enqueueCommitWrite is a convenience wrapper for a single op.
func enqueueCommitWrite(f readerWriterAt, fsOffset int64, sb *superblock, group uint32, startAbs int64, data []byte) error {
	return enqueueCommitWrites(f, fsOffset, sb, group, []commitOp{{startAbs: startAbs, data: data}})
}
