//go:build test

package filesystem_ext4

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
)

// traceFocusIno, when non-zero, limits traceWriteAt to only report writes
// that overlap the specific inode's on-disk byte range. Set to 5032 for
// targeted debugging of the hostname truncation.
// traceFocusIno, when non-zero, limits traceWriteAt to only report writes
// that overlap the specific inode's on-disk byte range. Set to 5032 for
// targeted debugging of the hostname truncation.
// Default: no focused tracing. Use EXT4_TRACE_FOCUS to enable.
var traceFocusIno uint32 = 0

// When set by traceReadFilePath, these represent the exact on-disk byte
// range to watch for WriteAt operations. Using a byte-range filter avoids
// repeated on-disk BG descriptor reads and is more reliable across runs.
// Use atomics to avoid introducing data races from concurrent trace calls.
var traceFocusOffsetStart int64
var traceFocusOffsetEnd int64

// When true, always iterate all block groups and report any write that
// overlaps any inode-table region. Useful when focused tracing misses the
// offending writer; enables broader but noisy diagnostics.
// Disable broad inode-table scanning by default to avoid noisy panics;
// prefer a focused byte-range set for a specific inode under investigation.
var traceAllInodeTables bool = false

func init() {
	// Allow enabling a focused inode trace via env var for ad-hoc debugging.
	if v := os.Getenv("EXT4_TRACE_FOCUS"); v != "" {
		if x, err := strconv.ParseUint(v, 10, 32); err == nil {
			atomic.StoreUint32(&traceFocusIno, uint32(x))
			debugPrintf("DEBUG: EXT4_TRACE_FOCUS set to %d\n", x)
		}
	}
	if os.Getenv("EXT4_TRACE_ALL_INODE_TABLES") != "" {
		traceAllInodeTables = true
		debugPrintf("DEBUG: EXT4_TRACE_ALL_INODE_TABLES enabled\n")
	}
}

// traceInodeWrite logs inode writes and prints a stack trace when the inode
// size decreases or becomes zero. Included only in test builds.
func traceInodeWrite(f readerWriterAt, fsOffset int64, sb *superblock, ino uint32, oldSize, newSize uint64) {
	if oldSize == newSize {
		return
	}
	debugPrintf("DEBUG traceInodeWrite: ino=%d oldSize=%d newSize=%d\n", ino, oldSize, newSize)
	if oldSize > newSize || newSize == 0 {
		var buf [4096]byte
		n := runtime.Stack(buf[:], false)
		debugPrintf("DEBUG traceInodeWrite stack:\n%s\n", string(buf[:n]))
	}
}

// traceDirOp logs directory add/zero operations in test builds.
func traceDirOp(action string, dirIno uint32, childIno uint32, name string) {
	debugPrintf("DEBUG traceDirOp: %s parent=%d child=%d name=%q\n", action, dirIno, childIno, name)
	if name == "hostname" || name == "hosts" {
		var buf [4096]byte
		n := runtime.Stack(buf[:], false)
		debugPrintf("DEBUG traceDirOp stack:\n%s\n", string(buf[:n]))
	}
}

// traceSetSize logs inode size changes in test builds.
func traceSetSize(ino uint32, oldSize, newSize uint64) {
	if oldSize == newSize {
		return
	}
	debugPrintf("DEBUG traceSetSize: ino=%d oldSize=%d newSize=%d\n", ino, oldSize, newSize)
	if oldSize > newSize || newSize == 0 {
		var buf [4096]byte
		n := runtime.Stack(buf[:], false)
		debugPrintf("DEBUG traceSetSize stack:\n%s\n", string(buf[:n]))
	}
}

// traceRawBlockWrite logs raw block writes that touch inode table blocks and
// prints a stack trace so tests can identify code paths that modify inodes
// at the block level.
func traceRawBlockWrite(f readerWriterAt, fsOffset int64, sb *superblock, blockNum uint64, data []byte) {
	if sb == nil || sb.BlocksPerGroup == 0 {
		return
	}
	// Determine the block group for this block and inspect that group's
	// inode table range. If the write targets an inode-table block, print
	// diagnostics and the goroutine stack.
	g := uint32(blockNum / uint64(sb.BlocksPerGroup))
	if g >= sb.numBlockGroups() {
		return
	}
	d, err := readBGD(f, fsOffset, sb, g)
	if err != nil {
		return
	}
	inodeTableStart := d.InodeTableBlock
	// Number of blocks occupied by an inode table for the group.
	inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
	if blockNum >= inodeTableStart && blockNum < inodeTableStart+inodeTableBlocks {
		debugPrintf("DEBUG traceRawBlockWrite: block=%d group=%d inodeTableStart=%d inodeTableBlocks=%d\n", blockNum, g, inodeTableStart, inodeTableBlocks)
		var buf [4096]byte
		n := runtime.Stack(buf[:], false)
		debugPrintf("DEBUG traceRawBlockWrite stack:\n%s\n", string(buf[:n]))
		// Print a short sample of the data being written for context.
		max := len(data)
		if max > 128 {
			max = 128
		}
		debugPrintf("DEBUG traceRawBlockWrite data (first %d bytes): %q\n", max, string(data[:max]))
	}
}

// traceAllocInode logs inode allocations in test builds.
func traceAllocInode(f readerWriterAt, fsOffset int64, sb *superblock, ino uint32) {
	debugPrintf("DEBUG traceAllocInode: ino=%d\n", ino)
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	debugPrintf("DEBUG traceAllocInode stack:\n%s\n", string(buf[:n]))
}

// traceFreeInodeSlot logs inode slot frees in test builds.
func traceFreeInodeSlot(f readerWriterAt, fsOffset int64, sb *superblock, ino uint32) {
	debugPrintf("DEBUG traceFreeInodeSlot: ino=%d\n", ino)
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	debugPrintf("DEBUG traceFreeInodeSlot stack:\n%s\n", string(buf[:n]))
}

// traceWriteAt inspects arbitrary WriteAt operations and prints a stack trace
// when the written byte range overlaps any inode-table region. This helps
// detect writers that overwrite inode-table bytes without going through
// higher-level inode helpers.
func traceWriteAt(f readerWriterAt, fsOffset int64, sb *superblock, off int64, data []byte) {
	if sb == nil || sb.BlocksPerGroup == 0 || sb.InodesPerGroup == 0 {
		return
	}
	start := off
	end := off + int64(len(data))
	// If a focused byte-range has been set (via traceReadFilePath), only
	// log writes that overlap that exact byte range — unless
	// `traceAllInodeTables` is set, in which case we fallthrough to the
	// broader group-iteration logic below to catch any inode-table writes.
	if !traceAllInodeTables {
		startFocus := atomic.LoadInt64(&traceFocusOffsetStart)
		endFocus := atomic.LoadInt64(&traceFocusOffsetEnd)
		// If a focus inode is configured but offsets are not yet populated,
		// compute the on-disk byte range for that inode so we can filter
		// precisely without relying on unrelated reads to set the range.
		if startFocus == 0 && endFocus == 0 {
			focus := atomic.LoadUint32(&traceFocusIno)
			if focus != 0 {
				fg := (focus - 1) / sb.InodesPerGroup
				if fg < sb.numBlockGroups() {
					if d2, err := readBGD(f, fsOffset, sb, fg); err == nil {
						inodeTableStartOff := fsOffset + int64(d2.InodeTableBlock)*int64(sb.BlockSize)
						localIdx := (focus - 1) % sb.InodesPerGroup
						inodeOffset2 := inodeTableStartOff + int64(localIdx)*int64(sb.InodeSize)
						atomic.StoreInt64(&traceFocusOffsetStart, inodeOffset2)
						atomic.StoreInt64(&traceFocusOffsetEnd, inodeOffset2+int64(sb.InodeSize))
						startFocus = inodeOffset2
						endFocus = inodeOffset2 + int64(sb.InodeSize)
						debugPrintf("DEBUG traceWriteAt set focus ino=%d group=%d inodeOffset=%d inodeSize=%d\n", focus, fg, inodeOffset2, sb.InodeSize)
					}
				}
			}
		}
		if startFocus != 0 && endFocus != 0 {
			if start < endFocus && end > startFocus {
				debugPrintf("DEBUG traceWriteAt(filter offs=[%d,%d) write=[%d,%d)) overlaps target ino=%d\n", startFocus, endFocus, start, end, atomic.LoadUint32(&traceFocusIno))
				var buf [4096]byte
				m := runtime.Stack(buf[:], false)
				debugPrintf("DEBUG traceWriteAt stack:\n%s\n", string(buf[:m]))
				sample := data
				if len(sample) > 128 {
					sample = sample[:128]
				}
				debugPrintf("DEBUG traceWriteAt data (first %d bytes): %q\n", len(sample), string(sample))
			}
			return
		}
	}

	// Fallback: iterate all groups and report any inode-table overlap (legacy behavior).
	nGroups := sb.numBlockGroups()
	for g := uint32(0); g < nGroups; g++ {
		d, err := readBGD(f, fsOffset, sb, g)
		if err != nil {
			continue
		}
		inodeTableStartOff := fsOffset + int64(d.InodeTableBlock)*int64(sb.BlockSize)
		inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
		inodeTableEndOff := inodeTableStartOff + int64(inodeTableBlocks)*int64(sb.BlockSize)
		// Overlap check.
		if start < inodeTableEndOff && end > inodeTableStartOff {
			// Compute inode number range for this group's inode table.
			firstIno := g*sb.InodesPerGroup + 1
			lastIno := firstIno + sb.InodesPerGroup - 1
			debugPrintf("DEBUG traceWriteAt: write offs=[%d,%d) overlaps inode table group=%d start=%d blocks=%d inodeRange=[%d,%d]\n", start, end, g, inodeTableStartOff, inodeTableBlocks, firstIno, lastIno)
			// Determine which inode numbers this write actually touches.
			overlapStart := start
			if overlapStart < inodeTableStartOff {
				overlapStart = inodeTableStartOff
			}
			overlapEnd := end
			if overlapEnd > inodeTableEndOff {
				overlapEnd = inodeTableEndOff
			}
			if overlapStart < overlapEnd {
				firstHit := firstIno + uint32((overlapStart-inodeTableStartOff)/int64(sb.InodeSize))
				lastHit := firstIno + uint32((overlapEnd-inodeTableStartOff-1)/int64(sb.InodeSize))
				// If a focused inode is configured, skip reporting unless this
				// write touches that inode to avoid noisy logs.
				focus := atomic.LoadUint32(&traceFocusIno)
				if focus != 0 && (focus < firstHit || focus > lastHit) {
					// Not our target; continue scanning other groups.
					continue
				}
				debugPrintf("DEBUG traceWriteAt: write offs=[%d,%d) overlaps inode table group=%d start=%d blocks=%d inodeRange=[%d,%d]\n", start, end, g, inodeTableStartOff, inodeTableBlocks, firstIno, lastIno)
				debugPrintf("DEBUG traceWriteAt overlappedInoRange=[%d,%d]\n", firstHit, lastHit)
				// Print a small data sample for context.
				sample := data
				if len(sample) > 128 {
					sample = sample[:128]
				}
				debugPrintf("DEBUG traceWriteAt data (first %d bytes): %q\n", len(sample), string(sample))
				// Print all goroutine stacks (non-fatal) so we can inspect who wrote
				// into the inode table without crashing the test process.
				var full [1 << 16]byte
				m := runtime.Stack(full[:], true)
				debugPrintf("DEBUG traceWriteAt all goroutine stacks:\n%s\n", string(full[:m]))
			}
			return
		}
	}
}

// traceLookupParentResolved logs a stack trace when path lookups resolve a
// parent directory for interesting paths (e.g., paths under /etc or the
// filename "hostname").
func traceLookupParentResolved(f readerWriterAt, fsOffset int64, sb *superblock, path string, parentIno uint32, name string) {
	if !strings.Contains(path, "/etc") && name != "hostname" && name != "hosts" {
		return
	}
	debugPrintf("DEBUG traceLookupParentResolved: path=%q parentIno=%d name=%q\n", path, parentIno, name)
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	debugPrintf("DEBUG traceLookupParentResolved stack:\n%s\n", string(buf[:n]))
}

// traceReadFilePath prints diagnostic info when reading paths of interest
// (notably /etc/hostname or /etc/hosts) to map the inode number to the on-disk inode-table
// byte offset. This helps focus traceWriteAt on the correct byte range.
func traceReadFilePath(f readerWriterAt, fsOffset int64, sb *superblock, path string, ino uint32, size uint64) {
	if path != "/etc/hostname" && path != "/etc/hosts" {
		return
	}
	debugPrintf("DEBUG traceReadFilePath: path=%q ino=%d size=%d\n", path, ino, size)
	if sb == nil || sb.InodesPerGroup == 0 {
		return
	}
	g := (ino - 1) / sb.InodesPerGroup
	if g >= sb.numBlockGroups() {
		return
	}
	d, err := readBGD(f, fsOffset, sb, g)
	if err != nil {
		return
	}
	inodeTableStartOff := fsOffset + int64(d.InodeTableBlock)*int64(sb.BlockSize)
	localIdx := (ino - 1) % sb.InodesPerGroup
	inodeOffset := inodeTableStartOff + int64(localIdx)*int64(sb.InodeSize)
	debugPrintf("DEBUG traceReadFilePath: ino=%d group=%d inodeOffset=%d inodeSize=%d\n", ino, g, inodeOffset, sb.InodeSize)

	// If a global focus inode is configured but its byte-range hasn't been set,
	// compute and populate the focused byte-range for that inode. This allows
	// debugging of a specific inode (e.g., 5032) without being overwritten by
	// unrelated /etc reads that occur frequently during stress runs.
	if atomic.LoadInt64(&traceFocusOffsetStart) == 0 && atomic.LoadInt64(&traceFocusOffsetEnd) == 0 {
		focus := atomic.LoadUint32(&traceFocusIno)
		if focus != 0 {
			fg := (focus - 1) / sb.InodesPerGroup
			if fg < sb.numBlockGroups() {
				d2, err := readBGD(f, fsOffset, sb, fg)
				if err == nil {
					inodeTableStartOff := fsOffset + int64(d2.InodeTableBlock)*int64(sb.BlockSize)
					localIdx := (focus - 1) % sb.InodesPerGroup
					inodeOffset2 := inodeTableStartOff + int64(localIdx)*int64(sb.InodeSize)
					atomic.StoreInt64(&traceFocusOffsetStart, inodeOffset2)
					atomic.StoreInt64(&traceFocusOffsetEnd, inodeOffset2+int64(sb.InodeSize))
					debugPrintf("DEBUG traceReadFilePath set focus ino=%d group=%d inodeOffset=%d inodeSize=%d\n", focus, fg, inodeOffset2, sb.InodeSize)
				}
			}
		}
	}

	// Set the focused byte-range so traceWriteAt can filter precisely for the
	// inode of the current read only when the global focus is unset or when the
	// focus explicitly targets this inode.
	cur := atomic.LoadUint32(&traceFocusIno)
	if cur == 0 || cur == ino {
		atomic.StoreUint32(&traceFocusIno, ino)
		atomic.StoreInt64(&traceFocusOffsetStart, inodeOffset)
		atomic.StoreInt64(&traceFocusOffsetEnd, inodeOffset+int64(sb.InodeSize))
	}
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	debugPrintf("DEBUG traceReadFilePath stack:\n%s\n", string(buf[:n]))
}

// traceRename logs renames that involve the hostname filename or the /etc
// directory, printing a stack to help identify who triggered the rename.
func traceRename(f readerWriterAt, fsOffset int64, sb *superblock, oldParent, newParent uint32, oldName, newName string) {
	if oldName != "hostname" && oldName != "hosts" && newName != "hostname" && newName != "hosts" {
		// Also trigger when either parent is the known /etc inode used in tests
		// (inode 121 is observed in logs), but be conservative to avoid noise.
		if oldParent != 121 && newParent != 121 {
			return
		}
	}
	debugPrintf("DEBUG traceRename: oldParent=%d newParent=%d oldName=%q newName=%q\n", oldParent, newParent, oldName, newName)
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	debugPrintf("DEBUG traceRename stack:\n%s\n", string(buf[:n]))
}

// traceDecrementAndFreeInode logs when an inode's link count is decremented
// and the inode is freed (newLinks == 0). It prints the stack for the
// affected inode, especially if it matches suspicious inode numbers.
func traceDecrementAndFreeInode(f readerWriterAt, fsOffset int64, sb *superblock, ino uint32, oldLinks, newLinks uint16) {
	if newLinks != 0 && ino != 5032 {
		return
	}
	debugPrintf("DEBUG traceDecrementAndFreeInode: ino=%d oldLinks=%d newLinks=%d\n", ino, oldLinks, newLinks)
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	debugPrintf("DEBUG traceDecrementAndFreeInode stack:\n%s\n", string(buf[:n]))
}

// traceMemFileWriteAt logs all HookMemFile WriteAt calls in test builds. This
// helps capture the exact goroutine and call stack for any in-memory writes
// that might be bypassing higher-level write instrumentation.
func traceMemFileWriteAt(f readerWriterAt, off int64, data []byte) {
	// Be conservative: only log moderately sized writes to avoid excessive noise.
	if len(data) < 8 {
		return
	}
	debugPrintf("DEBUG traceMemFileWriteAt: off=%d len=%d\n", off, len(data))
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	debugPrintf("DEBUG traceMemFileWriteAt stack:\n%s\n", string(buf[:n]))
	sample := data
	if len(sample) > 128 {
		sample = sample[:128]
	}
	debugPrintf("DEBUG traceMemFileWriteAt data (first %d bytes): %q\n", len(sample), string(sample))
}
