//go:build test
// +build test

package filesystem_ext4

import (
	"os"
	"runtime"
	"sync/atomic"
)

// loggingRW wraps a blockDevice and intercepts WriteAt to call tracing hooks
// in test builds so we can observe any WriteAt that would otherwise bypass
// higher-level instrumentation.
type loggingRW struct {
	f        blockDevice
	fsOffset int64
	sb       *superblock
}

func (l *loggingRW) ReadAt(p []byte, off int64) (int, error) {
	return l.f.ReadAt(p, off)
}

func (l *loggingRW) WriteAt(p []byte, off int64) (int, error) {
	// Trace before delegating; traceWriteAt is conservative and will
	// inspect inode-table overlaps when sb is present.
	traceWriteAt(l.f, l.fsOffset, l.sb, off, p)
	// Test-only: detect and warn if this WriteAt overlaps any inode-table
	// region and the corresponding per-group inode-table lock does not
	// appear to be held. This can surface callers that bypass the
	// centralized locking discipline.
	if l.sb != nil && l.sb.BlocksPerGroup != 0 && l.sb.InodesPerGroup != 0 {
		start := off
		end := off + int64(len(p))
		nGroups := l.sb.numBlockGroups()
		for g := uint32(0); g < nGroups; g++ {
			d, err := readBGD(l.f, l.fsOffset, l.sb, g)
			if err != nil {
				continue
			}
			inodeTableStartOff := l.fsOffset + int64(d.InodeTableBlock)*int64(l.sb.BlockSize)
			inodeTableBlocks := (uint64(l.sb.InodeSize)*uint64(l.sb.InodesPerGroup) + uint64(l.sb.BlockSize) - 1) / uint64(l.sb.BlockSize)
			inodeTableEndOff := inodeTableStartOff + int64(inodeTableBlocks)*int64(l.sb.BlockSize)
			if start < inodeTableEndOff && end > inodeTableStartOff {
				// Check whether the per-group inode-table lock exists and
				// appears held (owner != 0). This is a best-effort check
				// useful for diagnostics in tests.
				key := inodeTableLockKey(l, g)
				inodeTableGroupLocksMu.Lock()
				m := inodeTableGroupLocks[key]
				inodeTableGroupLocksMu.Unlock()
				held := false
				if m != nil {
					if atomic.LoadUint64(&m.owner) != 0 {
						held = true
					}
				}
				if !held {
					firstIno := g*l.sb.InodesPerGroup + 1
					lastIno := firstIno + l.sb.InodesPerGroup - 1
					debugPrintf("WARN Unprotected WriteAt: offs=[%d,%d) overlaps inode table group=%d inodeRange=[%d,%d] fileKey=%s\n", start, end, g, firstIno, lastIno, fileLockKey(l))
					var buf [1 << 12]byte
					n := runtime.Stack(buf[:], true)
					debugPrintf("WARN Unprotected WriteAt stacks:\n%s\n", string(buf[:n]))
				}
			}
		}
	}
	// Route through centralized helper to ensure tracing and inode-table
	// group locking are applied even when callers invoke the wrapper's
	// WriteAt directly (test-only). Use the underlying blockDevice so
	// writeAtWithTrace performs IO via the device and avoids recursive calls.
	if l.f != nil {
		return writeAtWithTrace(l.f, l.fsOffset, l.sb, off, p)
	}
	return 0, nil
}

func getRW(fs *ext4FS) readerWriterAt {
	return &loggingRW{f: fs.f, fsOffset: fs.partOffset, sb: fs.sb}
}

// underlyingOSFile returns the *os.File backing this device if available,
// or nil for non-file-backed devices (e.g. qcow2).
func (l *loggingRW) underlyingOSFile() *os.File {
	type fileGetter interface{ UnderlyingFile() *os.File }
	if g, ok := l.f.(fileGetter); ok {
		return g.UnderlyingFile()
	}
	return nil
}

// UnderlyingFile exposes the wrapped *os.File for callers that need the
// underlying descriptor (used by locking helpers to key locks by fd).
func (l *loggingRW) UnderlyingFile() *os.File { return l.underlyingOSFile() }
