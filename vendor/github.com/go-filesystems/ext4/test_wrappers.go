//go:build test
// +build test

package filesystem_ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// Exported aliases to internal types so test packages can refer to them
type ReaderWriterAt = readerWriterAt
type Superblock = superblock
type Inode = inode
type Bgd = bgd
type ExtentLeaf = extentLeaf

// Expose the concrete FS type as an exported alias for tests.
type Ext4FS = ext4FS

// FormatFile is an alias to the internal formatFile interface used by Format.
type FormatFile = formatFile

// HookMemFile is a small in-memory reader/writer used by tests.
type HookMemFile struct {
	buf []byte
	mu  sync.RWMutex
	// Exported hook fields are convenient for external tests.
	ReadHook  func(off int64, p []byte) error
	WriteHook func(off int64, p []byte) error
	// Keep unexported aliases for package-local tests that may still set
	// `readHook`/`writeHook` directly.
	readHook  func(off int64, p []byte) error
	writeHook func(off int64, p []byte) error
}

func (m *HookMemFile) ReadAt(p []byte, off int64) (int, error) {
	if m.ReadHook != nil {
		if err := m.ReadHook(off, p); err != nil {
			return 0, err
		}
	}
	if m.readHook != nil {
		if err := m.readHook(off, p); err != nil {
			return 0, err
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if off < 0 || int(off) > len(m.buf) {
		return 0, errors.New("read past end")
	}
	n := copy(p, m.buf[off:])
	if n < len(p) {
		return n, errors.New("short read")
	}
	return n, nil
}

func (m *HookMemFile) WriteAt(p []byte, off int64) (int, error) {
	if m.WriteHook != nil {
		if err := m.WriteHook(off, p); err != nil {
			return 0, err
		}
	}
	if m.writeHook != nil {
		if err := m.writeHook(off, p); err != nil {
			return 0, err
		}
	}
	// Test-only: capture all in-memory WriteAt callers and print stack/data.
	// Run tracing asynchronously and pass a copy of the buffer so the
	// instrumentation may call ReadAt without deadlocking the caller
	// (trace helpers may call ReadAt which acquires the same RWMutex).
	data := append([]byte(nil), p...)
	go traceMemFileWriteAt(m, off, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(off)+len(p) > len(m.buf) {
		grow := make([]byte, int(off)+len(p))
		copy(grow, m.buf)
		m.buf = grow
	}
	n := copy(m.buf[off:], p)
	return n, nil
}

// CloneSuperblock returns a deep copy of a superblock.
func CloneSuperblock(sb *Superblock) *Superblock {
	cp := *sb
	if sb.raw != nil {
		cp.raw = append([]byte(nil), sb.raw...)
	}
	return &cp
}

// NewTestSuperblock builds an in-memory superblock for tests.
func NewTestSuperblock(inodesPerGroup, blocksPerGroup, numGroups uint32, blockSize uint32) *Superblock {
	blockCount := uint64(numGroups) * uint64(blocksPerGroup)
	sb := &superblock{
		raw:            make([]byte, 1024),
		InodesPerGroup: inodesPerGroup,
		BlocksPerGroup: blocksPerGroup,
		BlocksCount:    blockCount,
		BlockSize:      blockSize,
		DescSize:       32,
	}
	return sb
}

// Export inode offset constants used by tests.
var (
	InodeOffMode       = inodeOffMode
	InodeOffSizeLo     = inodeOffSizeLo
	InodeOffLinksCount = inodeOffLinksCount
	InodeOffFlags      = inodeOffFlags
	InodeOffBlock      = inodeOffBlock
	InodeOffGeneration = inodeOffGeneration
	InodeOffFilACLLo   = inodeOffFilACLLo
	InodeOffSizeHi     = inodeOffSizeHi
	InodeOffBlocksHi   = inodeOffBlocksHi
	InodeOffCsumLo     = inodeOffCsumLo
	InodeOffExtraIsize = inodeOffExtraIsize
	InodeOffCsumHi     = inodeOffCsumHi
)

// NewTempFS creates and returns a temporary formatted ext4 filesystem for tests.
// The returned cleanup function should be deferred by the caller.
func NewTempFS(t *testing.T) (*Ext4FS, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "ext4test")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	// Use a larger default image for tests to avoid allocation exhaustion
	// during heavy concurrent test workloads.
	fsi, err := Format(path, 4096*4096, FormatConfig{})
	if err != nil {
		os.Remove(path)
		t.Fatal(err)
	}
	fs, ok := fsi.(*ext4FS)
	if !ok {
		os.Remove(path)
		t.Fatal("Format returned non-ext4 implementation")
	}
	return fs, func() { fs.Close(); os.Remove(path) }
}

// NewTempFSWithSize creates and returns a temporary formatted ext4 filesystem
// for tests with a custom image size in bytes. The returned cleanup
// function should be deferred by the caller.
func NewTempFSWithSize(t *testing.T, sizeBytes int64) (*Ext4FS, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "ext4test")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	fsi, err := Format(path, sizeBytes, FormatConfig{})
	if err != nil {
		os.Remove(path)
		t.Fatal(err)
	}
	fs, ok := fsi.(*ext4FS)
	if !ok {
		os.Remove(path)
		t.Fatal("Format returned non-ext4 implementation")
	}
	return fs, func() { fs.Close(); os.Remove(path) }
}

// CloneFSImage reads the backing file for an open FS into a HookMemFile copy.
func CloneFSImage(t *testing.T, fs *Ext4FS) *HookMemFile {
	t.Helper()
	type nameGetter interface{ Name() string }
	ng, ok := fs.f.(nameGetter)
	if !ok {
		t.Fatal("CloneFSImage: backing device does not expose Name()")
	}
	data, err := os.ReadFile(ng.Name())
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", ng.Name(), err)
	}
	return &HookMemFile{buf: append([]byte(nil), data...)}
}

// NewHookMemFile returns a HookMemFile backed by the provided buffer. This is
// a convenience for external tests that need a HookMemFile with an initial
// backing store.
func NewHookMemFile(buf []byte) *HookMemFile {
	return &HookMemFile{buf: buf}
}

// HookMemFileBuf returns the underlying buffer for tests that need to mutate
// or inspect the raw bytes inside a HookMemFile.
func HookMemFileBuf(h *HookMemFile) []byte { return h.buf }

// ReadRawBlock exposes readRawBlock.
func ReadRawBlock(f ReaderWriterAt, fsOffset int64, sb *Superblock, blockNum uint64) ([]byte, error) {
	return readRawBlock(f, fsOffset, sb, blockNum)
}

// WriteRawBlock exposes writeRawBlock.
func WriteRawBlock(f ReaderWriterAt, fsOffset int64, sb *Superblock, blockNum uint64, data []byte) error {
	return writeRawBlock(f, fsOffset, sb, blockNum, data)
}

// ReadInode exposes readInode.
func ReadInode(f ReaderWriterAt, fsOffset int64, sb *Superblock, inodeNum uint32) (*Inode, error) {
	return readInode(f, fsOffset, sb, inodeNum)
}

// WriteInode exposes writeInode.
func WriteInode(f ReaderWriterAt, fsOffset int64, sb *Superblock, in *Inode) error {
	return writeInode(f, fsOffset, sb, in)
}

// WriteFileRaw exposes the internal writeFile helper used by package tests.
func WriteFileRaw(f ReaderWriterAt, fsOffset int64, sb *Superblock, path string, data []byte, perm os.FileMode) error {
	j := journalForAny(f)
	return writeFile(f, f, j, fsOffset, sb, path, data, perm)
}

// AllocInode exposes allocInode.
func AllocInode(f ReaderWriterAt, fsOffset int64, sb *Superblock, isDir bool) (uint32, error) {
	return allocInode(f, fsOffset, sb, isDir)
}

// AllocBlocks exposes allocBlocks.
func AllocBlocks(f ReaderWriterAt, fsOffset int64, sb *Superblock, n uint32) ([]uint64, error) {
	return allocBlocks(f, fsOffset, sb, n)
}

// FreeBlock exposes freeBlock.
func FreeBlock(f ReaderWriterAt, fsOffset int64, sb *Superblock, block uint64) error {
	return freeBlock(f, fsOffset, sb, block)
}

// WriteBGD exposes writeBGD.
func WriteBGD(f ReaderWriterAt, fsOffset int64, sb *Superblock, g uint32, d *Bgd) error {
	return writeBGD(f, fsOffset, sb, g, d)
}

// WriteBitmapWithCsum exposes writeBitmapWithCsum.
func WriteBitmapWithCsum(f ReaderWriterAt, fsOffset int64, sb *Superblock, g uint32, d *Bgd, isBlockBitmap bool) error {
	return writeBitmapWithCsum(f, fsOffset, sb, g, d, isBlockBitmap)
}

// FreeInodeBlocks exposes freeInodeBlocks.
func FreeInodeBlocks(f ReaderWriterAt, fsOffset int64, sb *Superblock, in *Inode) error {
	return freeInodeBlocks(f, fsOffset, sb, in)
}

// FreeInodeSlot exposes freeInodeSlot.
func FreeInodeSlot(f ReaderWriterAt, fsOffset int64, sb *Superblock, inodeNum uint32) error {
	return freeInodeSlot(f, fsOffset, sb, inodeNum)
}

// DecrementAndFreeInode exposes decrementAndFreeInode.
func DecrementAndFreeInode(f ReaderWriterAt, fsOffset int64, sb *Superblock, in *Inode) error {
	return decrementAndFreeInode(f, fsOffset, sb, in)
}

// ZeroDirEntry exposes zeroDirEntry.
func ZeroDirEntry(f ReaderWriterAt, fsOffset int64, sb *Superblock, dirIno *Inode, name string) error {
	return zeroDirEntry(f, fsOffset, sb, dirIno, name)
}

// ZeroEntryInBlock exposes zeroEntryInBlock.
func ZeroEntryInBlock(buf []byte, name string, le binary.ByteOrder) bool {
	return zeroEntryInBlock(buf, name, le)
}

// ParseExtentNode exposes parseExtentNode.
func ParseExtentNode(f ReaderWriterAt, fsOffset int64, sb *Superblock, buf []byte, inodeNum uint32, inodeRaw []byte) ([]ExtentLeaf, error) {
	exts, err := parseExtentNode(f, fsOffset, sb, buf, inodeNum, inodeRaw)
	if exts == nil {
		return nil, err
	}
	out := make([]ExtentLeaf, len(exts))
	for i := range exts {
		out[i] = exts[i]
	}
	return out, err
}

// CRC32c exposes crc32c.
func CRC32c(crc uint32, data []byte) uint32 { return crc32c(crc, data) }

// Bitmap/bit helpers.
func SetBit(b []byte, i int)                             { setBit(b, i) }
func ClearBit(b []byte, i int)                           { clearBit(b, i) }
func FindFreeBit(b []byte, maxBits int) (int, bool)      { return findFreeBit(b, maxBits) }
func FindFreeRun(b []byte, maxBits, n int) ([]int, bool) { return findFreeRun(b, maxBits, n) }

// BuildExtents converts phys block list into exported ExtentLeaf slice.
func BuildExtents(phys []uint64) []ExtentLeaf {
	in := buildExtents(phys)
	if in == nil {
		return nil
	}
	out := make([]ExtentLeaf, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}

// Dirent helpers.
func MinDirentSize(nameLen int) int       { return minDirentSize(nameLen) }
func ParseDirBlock(buf []byte) []DirEntry { return parseDirBlock(buf) }
func WriteDirEntry(buf []byte, off int, ino uint32, recLen uint16, name string, fileType uint8) {
	writeDirEntry(buf, off, ino, recLen, name, fileType)
}
func TryInsertDirEntry(buf []byte, childIno uint32, name string, fileType uint8, needed int) bool {
	return tryInsertDirEntry(buf, childIno, name, fileType, needed)
}

// Superblock helpers.
func ReadSuperblock(f ReaderWriterAt, fsOffset int64) (*Superblock, error) {
	return readSuperblock(f, fsOffset)
}
func WriteSuperblock(f ReaderWriterAt, fsOffset int64, sb *Superblock) error {
	return writeSuperblock(f, fsOffset, sb)
}
func PartitionOffset(r io.ReaderAt, partIndex int) (int64, error) {
	return partitionOffset(r, partIndex)
}

// Exported superblock convenience accessors for external tests.
func NumBlockGroups(sb *Superblock) uint32 { return sb.numBlockGroups() }
func BgdTableBlock(sb *Superblock) uint64  { return sb.bgdTableBlock() }
func CsumSeed(sb *Superblock) uint32       { return sb.csumSeed() }

// DecodeBGD exposes decodeBGD.
func DecodeBGD(raw []byte, sb *Superblock) *Bgd { return decodeBGD(raw, sb) }

// ReadBitmap exposes readBitmap.
func ReadBitmap(f ReaderWriterAt, fsOffset int64, sb *Superblock, bitmapBlock uint64) ([]byte, error) {
	return readBitmap(f, fsOffset, sb, bitmapBlock)
}

// AddRangeToTx exposes addRangeToTx so external tests can prepare
// transaction entries using the same locking semantics as production
// callers. This prevents test-only preparations from bypassing
// per-block locking and triggering lost-update races.
func AddRangeToTx(tx *Transaction, f ReaderWriterAt, fsOffset int64, sb *Superblock, startAbs int64, data []byte) error {
	return addRangeToTx(tx, f, fsOffset, sb, startAbs, data)
}

// EncodeBGD encodes a decoded BGD into its raw descriptor bytes and returns
// the raw byte slice for inspection by external tests.
func EncodeBGD(d *Bgd, sb *Superblock) []byte {
	d.encode(sb)
	return d.raw
}
func BgdRaw(d *Bgd) []byte { return d.raw }

// UpdateDirBlockCsum exposes updateDirBlockCsum for tests.
func UpdateDirBlockCsum(buf []byte, sb *Superblock, dir *Inode) {
	updateDirBlockCsum(buf, sb, dir)
}

// WriteBitmapBuf exposes writeBitmapBuf for low-level bitmap testing.
func WriteBitmapBuf(f ReaderWriterAt, fsOffset int64, sb *Superblock, g uint32, d *Bgd, isBlock bool, bitmapBlock uint64, bmap []byte) error {
	return writeBitmapBuf(f, fsOffset, sb, g, d, isBlock, bitmapBlock, bmap)
}

// Inode helpers and accessors for tests.
func NewTestInode(num uint32, inodeSize uint16) *Inode {
	return &inode{raw: make([]byte, inodeSize), num: num}
}
func InodeRaw(in *Inode) []byte                    { return in.raw }
func InodeNum(in *Inode) uint32                    { return in.num }
func InodeSizeVal(in *Inode) uint64                { return in.size }
func SetSize(in *Inode, size uint64)               { in.setSize(size) }
func SetMode(in *Inode, mode uint16, links uint16) { in.setMode(mode, links) }
func IsRegular(in *Inode) bool                     { return in.isRegular() }
func IsDir(in *Inode) bool                         { return in.isDir() }
func IsSymlink(in *Inode) bool                     { return in.isSymlink() }

// computeInodeCsum wrapper
func ComputeInodeCsum(sb *Superblock, in *Inode) { computeInodeCsum(sb, in) }

// SetInlineExtents sets inline extents on an inode (wrapped method).
func SetInlineExtents(in *Inode, exts []ExtentLeaf) error {
	il := make([]extentLeaf, len(exts))
	for i := range exts {
		il[i] = exts[i]
	}
	return in.setInlineExtents(il)
}

// ReadFileData exposes readFileData.
func ReadFileData(f ReaderWriterAt, fsOffset int64, sb *Superblock, in *Inode) ([]byte, error) {
	return readFileData(f, fsOffset, sb, in)
}

// ReadSymlink exposes readSymlink.
func ReadSymlink(f ReaderWriterAt, fsOffset int64, sb *Superblock, in *Inode) (string, error) {
	return readSymlink(f, fsOffset, sb, in)
}

// AddDirEntry exposes addDirEntry.
func AddDirEntry(f ReaderWriterAt, fsOffset int64, sb *Superblock, dir *Inode, child uint32, name string, fileType uint8) error {
	return addDirEntry(f, fsOffset, sb, dir, child, name, fileType)
}

// LookupPath exposes lookupPath.
func LookupPath(f ReaderWriterAt, fsOffset int64, sb *Superblock, path string) (*Inode, error) {
	return lookupPath(f, fsOffset, sb, path)
}

// ReadDir exposes readDir for tests that need to enumerate directory entries
// from a single inode.
func ReadDir(f ReaderWriterAt, fsOffset int64, sb *Superblock, dirIno *Inode) ([]DirEntry, error) {
	return readDir(f, fsOffset, sb, dirIno)
}

// LookupParent exposes lookupParent for tests validating parent resolution.
func LookupParent(f ReaderWriterAt, fsOffset int64, sb *Superblock, path string) (*Inode, string, error) {
	return lookupParent(f, fsOffset, sb, path)
}

// UpdateDotDot exposes the internal updateDotDot helper for tests that need
// to exercise updating '..' directory entries.
func UpdateDotDot(f ReaderWriterAt, fsOffset int64, sb *Superblock, dirIno *Inode, newParentIno uint32) error {
	return updateDotDot(f, fsOffset, sb, dirIno, newParentIno)
}

// ReadBGD exposes readBGD.
func ReadBGD(f ReaderWriterAt, fsOffset int64, sb *Superblock, g uint32) (*Bgd, error) {
	return readBGD(f, fsOffset, sb, g)
}

// BgdOffset exposes bgdOffset.
func BgdOffset(sb *Superblock, g uint32) int64 {
	return bgdOffset(sb, g)
}

// InodeOffsetFor computes inode byte offset (helper for tests).
func InodeOffsetFor(r ReaderWriterAt, sb *Superblock, inodeNum uint32) int64 {
	idx := inodeNum - 1
	g := idx / sb.InodesPerGroup
	localIdx := idx % sb.InodesPerGroup
	d, err := readBGD(r, 0, sb, g)
	if err != nil {
		return -1
	}
	return int64(d.InodeTableBlock)*int64(sb.BlockSize) + int64(localIdx)*int64(sb.InodeSize)
}

// CloneFSImageFromPath reads the image file into a buffer copy for in-memory tests.
func CloneFSImageFromPath(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), data...), nil
}

// Expose partition constants for tests.
var LinuxPartTypeGPT = linuxPartTypeGPT
var SectorSize = sectorSize

// CloneSuperblockFromFS returns a copy of the open filesystem's superblock.
func CloneSuperblockFromFS(fs *Ext4FS) *Superblock {
	return CloneSuperblock(fs.sb)
}

// RemoveFile exposes removeFile for tests that operate on low-level images.
func RemoveFile(f ReaderWriterAt, fsOffset int64, sb *Superblock, path string) error {
	return removeFile(f, fsOffset, sb, path)
}

// RemoveDir exposes removeDir for tests that operate on low-level images.
func RemoveDir(f ReaderWriterAt, fsOffset int64, sb *Superblock, path string) error {
	return removeDir(f, fsOffset, sb, path)
}

// DirBlockForPath returns the first data block for the given path.
func DirBlockForPath(t *testing.T, rw ReaderWriterAt, sb *Superblock, path string) uint64 {
	t.Helper()
	dir, err := lookupPath(rw, 0, sb, path)
	if err != nil {
		t.Fatalf("lookupPath(%q): %v", path, err)
	}
	exts, err := dir.extents(rw, 0, sb)
	if err != nil {
		t.Fatalf("dir.extents: %v", err)
	}
	if len(exts) == 0 {
		t.Fatalf("inode %d has no extents", dir.num)
	}
	return exts[0].PhysBlock
}

// RootDirBlockFor returns the first data block for the root directory.
func RootDirBlockFor(t *testing.T, rw ReaderWriterAt, sb *Superblock) uint64 {
	t.Helper()
	root, err := ReadInode(rw, 0, sb, RootIno)
	if err != nil {
		t.Fatalf("readInode(root): %v", err)
	}
	exts, err := root.extents(rw, 0, sb)
	if err != nil {
		t.Fatalf("root.extents: %v", err)
	}
	if len(exts) == 0 {
		t.Fatalf("root has no extents")
	}
	return exts[0].PhysBlock
}

// AddFastSymlink creates a symlink inode and dir entry in the live filesystem
// (used by tests that need a quick symlink creation helper).
func AddFastSymlink(t *testing.T, fs *Ext4FS, parentPath, name, target string) {
	t.Helper()
	var parent *inode
	var err error
	if parentPath == "/" {
		parent, err = readInode(fs.f, fs.partOffset, fs.sb, RootIno)
	} else {
		parent, err = lookupPath(fs.f, fs.partOffset, fs.sb, parentPath)
	}
	if err != nil {
		t.Fatalf("resolve parent %q: %v", parentPath, err)
	}

	inoNum, err := allocInode(fs.f, fs.partOffset, fs.sb, false)
	if err != nil {
		t.Fatalf("allocInode symlink: %v", err)
	}
	in := &inode{raw: make([]byte, fs.sb.InodeSize), num: inoNum}
	in.setMode(0xA1FF, 1)
	in.setSize(uint64(len(target)))
	copy(in.raw[inodeOffBlock:], []byte(target))
	if err := writeInode(fs.f, fs.partOffset, fs.sb, in); err != nil {
		t.Fatalf("writeInode symlink: %v", err)
	}
	if err := addDirEntry(fs.f, fs.partOffset, fs.sb, parent, in.num, name, FtSymlink); err != nil {
		t.Fatalf("addDirEntry symlink: %v", err)
	}
}

// Setter helpers for package-level hooks used by tests.
func SetRemoveDirChildDir(fn func(ReaderWriterAt, int64, *Superblock, string) error) func(ReaderWriterAt, int64, *Superblock, string) error {
	old := removeDirChildDir
	removeDirChildDir = fn
	return old
}

func SetRemoveDirChildFile(fn func(ReaderWriterAt, int64, *Superblock, string) error) func(ReaderWriterAt, int64, *Superblock, string) error {
	old := removeDirChildFile
	removeDirChildFile = fn
	return old
}

func SetRemoveDirFreeBlocks(fn func(ReaderWriterAt, int64, *Superblock, *Inode) error) func(ReaderWriterAt, int64, *Superblock, *Inode) error {
	old := removeDirFreeBlocks
	removeDirFreeBlocks = fn
	return old
}

func SetRemoveDirFreeSlot(fn func(ReaderWriterAt, int64, *Superblock, uint32) error) func(ReaderWriterAt, int64, *Superblock, uint32) error {
	old := removeDirFreeSlot
	removeDirFreeSlot = fn
	return old
}

func SetReadLinkReadInode(fn func(ReaderWriterAt, int64, *Superblock, uint32) (*Inode, error)) func(ReaderWriterAt, int64, *Superblock, uint32) (*Inode, error) {
	old := readLinkReadInode
	readLinkReadInode = fn
	return old
}

func SetWriteFileSetInlineExtents(fn func(*Inode, []extentLeaf) error) func(*Inode, []extentLeaf) error {
	old := writeFileSetInlineExtents
	writeFileSetInlineExtents = fn
	return old
}

// EnqueueRawCommitOps exposes a thin wrapper allowing tests to enqueue a
// multi-op commit where each op is specified by a start offset (relative to
// fsOffset) and a data slice. This is useful for constructing a single
// commit that contains both a bitmap write and a BGD write so the worker's
// `lastBitmap` optimization can be exercised in tests.
func EnqueueRawCommitOps(f ReaderWriterAt, fsOffset int64, sb *Superblock, group uint32, starts []int64, datas [][]byte) (chan error, uint64, error) {
	if len(starts) != len(datas) {
		return nil, 0, fmt.Errorf("starts/datas length mismatch")
	}
	ops := make([]commitOp, len(starts))
	for i := range starts {
		ops[i] = commitOp{startAbs: fsOffset + starts[i], data: datas[i]}
	}
	return enqueueCommitWritesUnderLock(f, fsOffset, sb, group, ops)
}

// Test-only helpers to observe and control ack->seq mapping behavior.
// GetSeqForAck returns the registered seq for the provided ack channel
// and a boolean indicating whether the mapping exists.
func GetSeqForAck(ack chan error) (uint64, bool) { return getSeqForAck(ack) }

// SetAckToSeqDeleteGraceMS sets the test-only ack->seq deletion grace
// duration in milliseconds. Useful for exercising TTL behavior in tests.
func SetAckToSeqDeleteGraceMS(ms int) { ackToSeqDeleteGrace = time.Duration(ms) * time.Millisecond }

func SetMakeDirAllocInode(fn func(ReaderWriterAt, int64, *Superblock, bool) (uint32, error)) func(ReaderWriterAt, int64, *Superblock, bool) (uint32, error) {
	old := makeDirAllocInode
	makeDirAllocInode = fn
	return old
}

func SetMakeDirAllocBlocks(fn func(ReaderWriterAt, int64, *Superblock, uint32) ([]uint64, error)) func(ReaderWriterAt, int64, *Superblock, uint32) ([]uint64, error) {
	old := makeDirAllocBlocks
	makeDirAllocBlocks = fn
	return old
}

// MakeDir exposes the internal makeDir helper for external tests.
func MakeDir(f ReaderWriterAt, fsOffset int64, sb *Superblock, path string, perm os.FileMode) error {
	return makeDir(f, fsOffset, sb, path, perm)
}

// Rename exposes the internal rename helper for external tests.
func Rename(f ReaderWriterAt, fsOffset int64, sb *Superblock, oldPath, newPath string) error {
	return rename(f, fsOffset, sb, oldPath, newPath)
}

// Setters for addDirEntry package-level hooks so external tests can inject
// failures or behavior.
func SetAddDirEntryAllocBlocks(fn func(ReaderWriterAt, int64, *Superblock, uint32) ([]uint64, error)) func(ReaderWriterAt, int64, *Superblock, uint32) ([]uint64, error) {
	old := addDirEntryAllocBlocks
	addDirEntryAllocBlocks = fn
	return old
}

func SetAddDirEntryWriteBlock(fn func(ReaderWriterAt, int64, *Superblock, uint64, []byte) error) func(ReaderWriterAt, int64, *Superblock, uint64, []byte) error {
	old := addDirEntryWriteBlock
	addDirEntryWriteBlock = fn
	return old
}

func SetAddDirEntrySetInlineExtents(fn func(*Inode, []extentLeaf) error) func(*Inode, []extentLeaf) error {
	old := addDirEntrySetInlineExtents
	addDirEntrySetInlineExtents = fn
	return old
}

// Format hook setters - allow external tests to inject file/open/rand behavior.
func SetFormatOpenFile(fn func(string) (FormatFile, error)) func(string) (FormatFile, error) {
	old := formatOpenFile
	formatOpenFile = fn
	return old
}

func SetFormatRandRead(fn func([]byte) (int, error)) func([]byte) (int, error) {
	old := formatRandRead
	formatRandRead = fn
	return old
}

func SetFormatOpenFS(fn func(string, int) (filesystem.Filesystem, error)) func(string, int) (filesystem.Filesystem, error) {
	old := formatOpenFS
	formatOpenFS = fn
	return old
}

// Test helpers for commit dispatcher internals.
func MarkSeqAcked(seq uint64)    { markSeqAcked(seq) }
func IsSeqAcked(seq uint64) bool { return isSeqAcked(seq) }

// CreateFullWorker inserts a commitWorker for the provided file/group with
// a tasks channel of the given capacity and pre-fills it so enqueue attempts
// will hit the queue-full fallback path. Returns an error if a worker
// already exists for the computed key.
func CreateFullWorker(f ReaderWriterAt, fsOffset int64, sb *Superblock, group uint32, capacity int) error {
	if capacity <= 0 {
		capacity = 1
	}
	key := fmt.Sprintf("%s:g:%d", fileLockKey(f), group)
	commitWorkersMu.Lock()
	defer commitWorkersMu.Unlock()
	if _, ok := commitWorkers[key]; ok {
		// Idempotent: if a worker already exists, treat as success to
		// avoid races in tests that create/remove injected workers.
		return nil
	}
	w := &commitWorker{tasks: make(chan *commitTask, capacity), key: key}
	// pre-fill channel so it's full
	for i := 0; i < capacity; i++ {
		w.tasks <- &commitTask{ack: make(chan error, 1)}
	}
	commitWorkers[key] = w
	return nil
}

// RemoveWorkerFor removes any injected worker for the given file/group.
func RemoveWorkerFor(f ReaderWriterAt, fsOffset int64, sb *Superblock, group uint32) {
	key := fmt.Sprintf("%s:g:%d", fileLockKey(f), group)
	commitWorkersMu.Lock()
	delete(commitWorkers, key)
	commitWorkersMu.Unlock()
}

// WorkerExistsFor returns true if a worker is registered for the file/group.
func WorkerExistsFor(f ReaderWriterAt, fsOffset int64, sb *Superblock, group uint32) bool {
	key := fmt.Sprintf("%s:g:%d", fileLockKey(f), group)
	commitWorkersMu.Lock()
	_, ok := commitWorkers[key]
	commitWorkersMu.Unlock()
	return ok
}

// Get/Set commit worker idle duration (seconds) for tests.
func GetCommitWorkerIdleSecs() int  { return int(commitWorkerIdle / time.Second) }
func SetCommitWorkerIdleSecs(s int) { commitWorkerIdle = time.Duration(s) * time.Second }
