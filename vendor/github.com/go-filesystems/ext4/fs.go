// Package ext4 provides read/write access to ext4 filesystem images.
// See ext4.go for exported types and constants.
package filesystem_ext4

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

var readLinkReadInode = readInode

// readerWriterAt combines io.ReaderAt and io.WriterAt.
type readerWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

// FS represents an open ext4 filesystem image.
type ext4FS struct {
	f          blockDevice
	partOffset int64 // byte offset to the start of the ext4 partition
	sb         *superblock
	journal    *Journal
	mu         sync.RWMutex
}

// Verify implementation of the common filesystem interface.
var _ filesystem.Filesystem = (*ext4FS)(nil)

// (interface conformance check removed for tests)

// Open opens an ext4 filesystem image at imagePath, automatically detecting
// the partition table (MBR / GPT) and using the first Linux partition.
// Pass partIndex = -1 for auto-detection (first Linux partition).
func Open(imagePath string, partIndex int) (filesystem.Filesystem, error) {
	raw, err := os.OpenFile(imagePath, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ext4: open %s: %w", imagePath, err)
	}
	return openFromDevice(&osFileDevice{f: raw}, partIndex)
}

// OpenFromDevice opens an ext4 filesystem backed by an arbitrary block device.
// dev must remain valid until the returned Filesystem is closed.
func OpenFromDevice(dev blockDevice, partIndex int) (filesystem.Filesystem, error) {
	return openFromDevice(dev, partIndex)
}

func openFromDevice(dev blockDevice, partIndex int) (filesystem.Filesystem, error) {
	// Device size lets the shared go-volumes/gpt parser bound the partition
	// table against the backing device (M2). Size() failing is non-fatal: pass
	// 0 so partitionOffset falls back to its internal overflow-safe ceiling.
	devSize, _ := dev.Size()
	off, err := partitionOffset(dev, partIndex, devSize)
	if err != nil {
		dev.Close()
		return nil, err
	}
	sb, err := readSuperblock(dev, off)
	if err != nil {
		dev.Close()
		return nil, err
	}
	// Try to open a sidecar journal (best-effort). If a journal is present
	// ReplayOnOpen will apply any committed transactions.
	j, _ := OpenJournal(dev, off, int(sb.BlockSize), sb)
	if j != nil && j.enabled {
		if err := j.ReplayOnOpen(); err != nil {
			j.js.Close()
			dev.Close()
			return nil, fmt.Errorf("ext4: journal replay: %w", err)
		}
	}
	return &ext4FS{f: dev, partOffset: off, sb: sb, journal: j}, nil
}

// Close releases resources held by the FS.
func (fs *ext4FS) Close() error {
	if fs.journal != nil {
		// Close sidecar journal (will unregister and stop sync workers).
		_ = fs.journal.Close()
	}
	return fs.f.Close()
}

// ReadFile reads and returns the contents of the regular file at the given
// absolute path within the filesystem.
func (fs *ext4FS) ReadFile(path string) ([]byte, error) {
	// Minimize time spent holding the FS-level RLock. Lookup the path to
	// determine the inode number while holding `fs.mu.RLock()` and then
	// release it before performing the actual data read under the finer
	// grained file/inode locks. This reduces contention with writers.
	fs.mu.RLock()
	rw := getRW(fs)
	in, err := lookupPath(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		fs.mu.RUnlock()
		debugPrintf("DEBUG ReadFile lookupPathErr ts=%d path=%q err=%v\n", time.Now().UnixNano(), path, err)
		return nil, err
	}
	// Record trace info based on path/inode — this is cheap and safe while
	// holding the RLock.
	traceReadFilePath(rw, fs.partOffset, fs.sb, path, in.num, in.size)
	if !in.isRegular() {
		fs.mu.RUnlock()
		return nil, fmt.Errorf("ext4: %q is not a regular file", path)
	}
	ino := in.num
	fs.mu.RUnlock()

	// Acquire a per-inode lock and re-read the inode while holding it so
	// we don't observe a freed or transient inode state. `readFileData`
	// will reuse the same owner token when present to avoid deadlocks.
	owner := NewOwner()
	il := getInodeLock(rw, ino)
	il.LockOwner(owner)
	defer il.UnlockOwner(owner)

	freshIn, err := readInode(rw, fs.partOffset, fs.sb, ino)
	if err != nil {
		return nil, err
	}
	data, err := readFileData(rw, fs.partOffset, fs.sb, freshIn)
	if err == nil && len(data) == 0 && (path == "/etc/hosts" || path == "/etc/hostname") {
		// Diagnostic: log details when the target path is unexpectedly empty.
		debugPrintf("DEBUG ReadFile empty: path=%q ino=%d size=%d flags=0x%08x\n", path, freshIn.num, freshIn.size, freshIn.flags())
		// Additional diagnostic: dump /etc directory entries and their inode sizes
		if parent, _, perr := lookupParent(rw, fs.partOffset, fs.sb, path); perr == nil {
			if ents, derr := readDir(rw, fs.partOffset, fs.sb, parent); derr == nil {
				found := false
				target := "hostname"
				if path == "/etc/hosts" {
					target = "hosts"
				}
				for _, e := range ents {
					if e.Name != target {
						continue
					}
					found = true
					child, cerr := readInode(rw, fs.partOffset, fs.sb, e.Inode)
					if cerr == nil {
						debugPrintf("DEBUG %s dirent: name=%q ino=%d size=%d flags=0x%08x\n", target, e.Name, e.Inode, child.size, child.flags())
					} else {
						debugPrintf("DEBUG %s dirent: name=%q ino=%d readInodeErr=%v\n", target, e.Name, e.Inode, cerr)
					}
					break
				}
				if !found {
					debugPrintf("DEBUG %s dirent: not found in /etc\n", target)
				}
				// Also check /etc/hosts for comparison when hostname is empty.
				if hostsIn, herr := lookupPath(rw, fs.partOffset, fs.sb, "/etc/hosts"); herr == nil {
					debugPrintf("DEBUG hosts inode: ino=%d size=%d flags=0x%08x\n", hostsIn.num, hostsIn.size, hostsIn.flags())
					if hostsData, herr2 := readFileData(rw, fs.partOffset, fs.sb, hostsIn); herr2 == nil {
						max := len(hostsData)
						if max > 128 {
							max = 128
						}
						debugPrintf("DEBUG /etc/hosts content (first %d bytes): %q\n", max, string(hostsData[:max]))
					} else {
						debugPrintf("DEBUG read /etc/hosts error: %v\n", herr2)
					}
				} else {
					debugPrintf("DEBUG /etc/hosts lookup error: %v\n", herr)
				}
			}
		}
	}
	return data, err
}

// ListDir returns the directory entries of the directory at the given absolute
// path.
func (fs *ext4FS) ListDir(path string) ([]filesystem.DirEntry, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	rw := getRW(fs)

	in, err := lookupPath(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		return nil, err
	}
	if !in.isDir() {
		return nil, fmt.Errorf("ext4: %q is not a directory", path)
	}
	entries, err := readDir(rw, fs.partOffset, fs.sb, in)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.DirEntry, 0, len(entries))
	for _, e := range entries {
		// Omit the "." and ".." self/parent links from listings, matching the
		// other go-filesystems drivers (and os.ReadDir): consumers that recurse
		// over ListDir would otherwise loop.
		if e.Name == "." || e.Name == ".." {
			continue
		}
		out = append(out, filesystem.NewDirEntry(uint64(e.Inode), e.Name, e.FileType))
	}
	return out, nil
}

// Stat holds basic metadata about a filesystem path.
type Stat struct {
	Mode  uint16 // Unix permission bits
	Size  uint64
	Inode uint32
}

// Stat returns basic metadata for the file or directory at path.
func (fs *ext4FS) Stat(path string) (filesystem.Stat, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	rw := getRW(fs)

	in, err := lookupPath(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(in.mode, in.size, uint64(in.num)), nil
}

// WriteFile creates or overwrites the file at the given absolute path with
// the supplied data and Unix permission bits.
// Parent directories must already exist.
func (fs *ext4FS) WriteFile(path string, data []byte, perm os.FileMode) error {
	// Avoid holding the global FS lock here to reduce contention under
	// heavy concurrent writers. The lower-level helpers (per-file locks,
	// per-inode locks, and group bitmap/BGD locks) provide the necessary
	// serialization for metadata updates.
	rw := getRW(fs)
	return writeFile(rw, fs.f, fs.journal, fs.partOffset, fs.sb, path, data, perm)
}

// ReadLink returns the target of the symbolic link at path without
// following the final path component.
func (fs *ext4FS) ReadLink(path string) (string, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if path == "/" {
		return "", fmt.Errorf("ext4: %q is not a symbolic link", path)
	}
	rw := getRW(fs)
	parent, name, err := lookupParent(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		return "", err
	}
	entries, err := readDir(rw, fs.partOffset, fs.sb, parent)
	if err != nil {
		return "", err
	}
	var in *inode
	for _, entry := range entries {
		if entry.Name != name {
			continue
		}
		in, err = readLinkReadInode(rw, fs.partOffset, fs.sb, entry.Inode)
		if err != nil {
			return "", err
		}
		break
	}
	if in == nil {
		return "", fmt.Errorf("ext4: %q not found", name)
	}
	if !in.isSymlink() {
		return "", fmt.Errorf("ext4: %q is not a symbolic link", path)
	}
	return readSymlink(rw, fs.partOffset, fs.sb, in)
}

// MkDir creates a new directory at path with the given Unix permission bits.
// The parent directory must already exist. Returns an error if path already
// exists.
func (fs *ext4FS) MkDir(path string, perm os.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	rw := getRW(fs)
	return makeDir(rw, fs.partOffset, fs.sb, path, perm)
}

// DeleteDir removes the directory at path and all of its contents
// recursively. Returns nil if path does not exist (idempotent).
func (fs *ext4FS) DeleteDir(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	rw := getRW(fs)
	return removeDir(rw, fs.partOffset, fs.sb, path)
}

// Rename moves (and optionally renames) the file or directory at oldPath to
// newPath. If newPath already exists it is replaced (directories must be
// empty).
func (fs *ext4FS) Rename(oldPath, newPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	rw := getRW(fs)
	return rename(rw, fs.partOffset, fs.sb, oldPath, newPath)
}

// readRawBlock reads one filesystem block (sb.BlockSize bytes) by absolute
// block number.
func readRawBlock(f readerWriterAt, fsOffset int64, sb *superblock, blockNum uint64) ([]byte, error) {
	buf := make([]byte, sb.BlockSize)
	off := fsOffset + int64(blockNum)*int64(sb.BlockSize)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("ext4: read block %d: %w", blockNum, err)
	}
	return buf, nil
}

// writeRawBlock writes one filesystem block by absolute block number.
func writeRawBlock(f readerWriterAt, fsOffset int64, sb *superblock, blockNum uint64, data []byte) error {
	// writeRawBlock delegates locking/ordering to writeAtWithTrace which
	// centralizes inode-table overlap detection under the BGD->inode
	// canonical ordering. Keeping duplicate checks here risks TOCTOU.

	// Test-only tracing: capture writes that touch inode table blocks so
	// tests can print stack traces when inode-table writes occur under stress.
	traceRawBlockWrite(f, fsOffset, sb, blockNum, data)

	// When a sidecar journal is present, route block writes through the
	// journal so that the write supersedes any earlier journaled snapshot
	// of the same block on replay. Without this, a direct WriteAt would be
	// silently clobbered when ReplayOnOpen reapplies a stale snapshot of
	// this block from a prior transaction. Callers that already manage a
	// Transaction should use addRangeToTx to group writes; callers that
	// don't (zeroDirEntry, updateDotDot, addDirEntryWriteBlock, etc.) get
	// the per-block local-tx via applyOrJournalWrite below.
	off := fsOffset + int64(blockNum)*int64(sb.BlockSize)
	if j := journalForAny(f); j != nil && j.enabled {
		if err := applyOrJournalWrite(f, fsOffset, sb, off, data); err != nil {
			return fmt.Errorf("ext4: write block %d: %w", blockNum, err)
		}
		return nil
	}
	if _, err := writeAtWithTrace(f, fsOffset, sb, off, data); err != nil {
		return fmt.Errorf("ext4: write block %d: %w", blockNum, err)
	}
	return nil
}

// writeAtWithTrace is a small wrapper used for call sites that write at an
// absolute offset. It invokes a test-only tracer that can inspect the byte
// range being written (for example to detect inode-table overwrites), and
// then performs the actual write.
func writeAtWithTrace(f readerWriterAt, fsOffset int64, sb *superblock, off int64, data []byte) (int, error) {
	// Emit test-only tracing first.
	traceWriteAt(f, fsOffset, sb, off, data)

	// If we have superblock info, detect writes that overlap inode-table
	// regions and acquire the per-group inode-table locks for those groups
	// before performing the actual WriteAt. To avoid TOCTOU races with
	// concurrent BGD preparation, acquire the per-group BGD lock while
	// inspecting the descriptor and then acquire the inode-table lock in
	// the canonical order (BGD -> inode-table) before releasing the BGD
	// lock. Iterate groups in increasing order to avoid cross-goroutine
	// deadlocks.
	var unlocks []func()
	if sb != nil && sb.BlocksPerGroup != 0 && sb.InodesPerGroup != 0 {
		start := off
		end := off + int64(len(data))
		nGroups := sb.numBlockGroups()

		// Build an initial candidate list of groups whose inode-tables
		// overlap the write range. Instead of scanning all groups, derive
		// the filesystem block range for the write and compute only the
		// groups touched by those blocks. This reduces the race window and
		// avoids unnecessary descriptor reads for unrelated groups.
		type cand struct {
			g               uint32
			inodeTableBlock uint64
		}
		var cands []cand
		bs := int64(sb.BlockSize)
		if end <= fsOffset {
			// write entirely before filesystem area
			cands = nil
		} else {
			// compute block range (clamp start to fsOffset)
			relStart := start - fsOffset
			if relStart < 0 {
				relStart = 0
			}
			relEnd := end - fsOffset
			if relEnd <= 0 {
				relEnd = 0
			}
			startBlock := uint64(relStart / bs)
			// end-1 to compute inclusive last block
			var endBlock uint64
			if relEnd == 0 {
				endBlock = startBlock
			} else {
				endBlock = uint64((relEnd - 1) / bs)
			}
			first := uint64(sb.FirstDataBlock)
			seen := map[uint32]struct{}{}
			for b := startBlock; b <= endBlock; b++ {
				if b < first || sb.BlocksPerGroup == 0 {
					continue
				}
				g := uint32((b - first) / uint64(sb.BlocksPerGroup))
				if g >= nGroups {
					continue
				}
				if _, ok := seen[g]; ok {
					continue
				}
				seen[g] = struct{}{}
				d, err := readBGD(f, fsOffset, sb, g)
				if err != nil {
					continue
				}
				inodeTableStartOff := fsOffset + int64(d.InodeTableBlock)*bs
				inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
				inodeTableEndOff := inodeTableStartOff + int64(inodeTableBlocks)*bs
				if start < inodeTableEndOff && end > inodeTableStartOff {
					cands = append(cands, cand{g: g, inodeTableBlock: d.InodeTableBlock})
				}
			}
		}

		if len(cands) > 0 {
			const maxAttempts = 8
			for attempt := 0; attempt < maxAttempts; attempt++ {
				// Acquire BGD locks for all candidate groups in increasing
				// order, then verify descriptors are stable, then acquire
				// inode-table locks in the same order. This ensures a
				// consistent canonical ordering (BGD -> inode) across
				// goroutines and reduces TOCTOU races for multi-group
				// writes.
				sort.Slice(cands, func(i, j int) bool { return cands[i].g < cands[j].g })
				unlocks = unlocks[:0]
				var unlockBGDs []func()
				unlockBGDs = unlockBGDs[:0]
				stable := true

				// Acquire all per-group BGD locks first.
				for _, c := range cands {
					unlockBGDs = append(unlockBGDs, lockBGDGroup(f, c.g))
				}

				// Verify descriptors are stable and ranges still overlap.
				for _, c := range cands {
					d2, err := readBGD(f, fsOffset, sb, c.g)
					if err != nil || d2.InodeTableBlock != c.inodeTableBlock {
						stable = false
						break
					}
					inodeTableStartOff := fsOffset + int64(d2.InodeTableBlock)*int64(sb.BlockSize)
					inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
					inodeTableEndOff := inodeTableStartOff + int64(inodeTableBlocks)*int64(sb.BlockSize)
					if !(start < inodeTableEndOff && end > inodeTableStartOff) {
						stable = false
						break
					}
				}

				if !stable {
					// Release all BGD locks and retry with backoff.
					for i := len(unlockBGDs) - 1; i >= 0; i-- {
						unlockBGDs[i]()
					}
					time.Sleep(time.Duration(50+attempt*20) * time.Microsecond)
					// rebuild candidate list and retry
					cands = cands[:0]
					for g := uint32(0); g < nGroups; g++ {
						d, err := readBGD(f, fsOffset, sb, g)
						if err != nil {
							continue
						}
						inodeTableStartOff := fsOffset + int64(d.InodeTableBlock)*int64(sb.BlockSize)
						inodeTableBlocks := (uint64(sb.InodeSize)*uint64(sb.InodesPerGroup) + uint64(sb.BlockSize) - 1) / uint64(sb.BlockSize)
						inodeTableEndOff := inodeTableStartOff + int64(inodeTableBlocks)*int64(sb.BlockSize)
						if start < inodeTableEndOff && end > inodeTableStartOff {
							cands = append(cands, cand{g: g, inodeTableBlock: d.InodeTableBlock})
						}
					}
					continue
				}

				// Acquire inode-table locks in order while still holding the
				// BGD locks, then release all BGD locks.
				for _, c := range cands {
					unlocks = append(unlocks, lockInodeTableGroup(f, c.g))
				}
				for i := len(unlockBGDs) - 1; i >= 0; i-- {
					unlockBGDs[i]()
				}

				// Stable: perform the write while holding inode-table locks.
				n, err := f.WriteAt(data, off)
				for i := len(unlocks) - 1; i >= 0; i-- {
					unlocks[i]()
				}
				return n, err
			}
			// Mapping remained unstable after retries: refuse to perform an
			// unprotected write which could corrupt inode tables and instead
			// return an error so callers must handle the failure safely.
			if len(unlocks) > 0 {
				for i := len(unlocks) - 1; i >= 0; i-- {
					unlocks[i]()
				}
			}
			debugPrintf("WARN writeAtWithTrace: unstable BGD mapping after retries; refusing unprotected WriteAt offs=[%d,%d)\n", off, off+int64(len(data)))
			return 0, fmt.Errorf("ext4: unstable BGD mapping after retries; refusing unprotected WriteAt offs=[%d,%d)", off, off+int64(len(data)))
		}
	}

	// Perform the write while holding any acquired inode-table locks.
	n, err := f.WriteAt(data, off)
	// Release locks in reverse order.
	for i := len(unlocks) - 1; i >= 0; i-- {
		unlocks[i]()
	}
	return n, err
}
