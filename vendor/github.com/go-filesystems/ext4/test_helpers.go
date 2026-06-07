package filesystem_ext4

import (
	"encoding/binary"
	"os"
	"testing"
)

// memFile is a simple in-memory ReaderAt/WriterAt for tests.
type memFile struct{ buf []byte }

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, m.buf[off:])
	if n < len(p) {
		return n, nil
	}
	return n, nil
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	if int(off)+len(p) > len(m.buf) {
		grow := make([]byte, int(off)+len(p))
		copy(grow, m.buf)
		m.buf = grow
	}
	n := copy(m.buf[off:], p)
	return n, nil
}

// hookMemFile mirrors the hookable in-memory file used by many tests.
type hookMemFile struct {
	buf       []byte
	readHook  func(off int64, p []byte) error
	writeHook func(off int64, p []byte) error
}

func (m *hookMemFile) ReadAt(p []byte, off int64) (int, error) {
	if m.readHook != nil {
		if err := m.readHook(off, p); err != nil {
			return 0, err
		}
	}
	if off < 0 || int(off) > len(m.buf) {
		return 0, os.ErrInvalid
	}
	n := copy(p, m.buf[off:])
	if n < len(p) {
		return n, os.ErrInvalid
	}
	return n, nil
}

func (m *hookMemFile) WriteAt(p []byte, off int64) (int, error) {
	if m.writeHook != nil {
		if err := m.writeHook(off, p); err != nil {
			return 0, err
		}
	}
	if int(off)+len(p) > len(m.buf) {
		grow := make([]byte, int(off)+len(p))
		copy(grow, m.buf)
		m.buf = grow
	}
	n := copy(m.buf[off:], p)
	return n, nil
}

// makeTestInode builds a minimal inode with the requested size.
func makeTestInode(num uint32, inodeSize uint16) *inode {
	in := &inode{raw: make([]byte, inodeSize), num: num}
	return in
}

// memReaderAt wraps a []byte as an io.ReaderAt (no writes needed here).
type memReaderAt []byte

func (m memReaderAt) ReadAt(p []byte, off int64) (int, error) {
	copy(p, m[off:])
	return len(p), nil
}

// rootDirBlockFor returns first data block for root directory (helper for
// package-local tests).
func rootDirBlockFor(t *testing.T, rw readerWriterAt, sb *superblock) uint64 {
	t.Helper()
	root, err := readInode(rw, 0, sb, RootIno)
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

// newTempFS creates a temporary formatted ext4 filesystem for package tests.

// cloneFSImage returns an in-memory copy of the backing file for an open FS.
func cloneFSImage(t *testing.T, fs *ext4FS) *hookMemFile {
	t.Helper()
	type nameGetter interface{ Name() string }
	ng, ok := fs.f.(nameGetter)
	if !ok {
		t.Fatal("cloneFSImage: backing device does not expose Name()")
	}
	data, err := os.ReadFile(ng.Name())
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", ng.Name(), err)
	}
	return &hookMemFile{buf: append([]byte(nil), data...)}
}

// cloneSuperblock returns a deep copy of the superblock.
func cloneSuperblock(sb *superblock) *superblock {
	cp := *sb
	if sb.raw != nil {
		cp.raw = append([]byte(nil), sb.raw...)
	}
	return &cp
}

// inodeOffsetFor computes the inode byte offset (helper for package tests).
func inodeOffsetFor(t *testing.T, r readerWriterAt, sb *superblock, inodeNum uint32) int64 {
	t.Helper()
	idx := inodeNum - 1
	g := idx / sb.InodesPerGroup
	localIdx := idx % sb.InodesPerGroup
	d, err := readBGD(r, 0, sb, g)
	if err != nil {
		return -1
	}
	return int64(d.InodeTableBlock)*int64(sb.BlockSize) + int64(localIdx)*int64(sb.InodeSize)
}

// bgdOffset computes block group descriptor offset.
func bgdOffset(sb *superblock, g uint32) int64 {
	tableBlock := sb.bgdTableBlock()
	return int64(tableBlock)*int64(sb.BlockSize) + int64(g)*int64(sb.DescSize)
}

// package-level compatibility shim: many existing tests call addFastSymlink
// (lowercase). Provide a thin wrapper that forwards to the exported
// AddFastSymlink so those tests continue to compile while we migrate files.
func addFastSymlink(t *testing.T, fs *ext4FS, parentPath, name, target string) {
	t.Helper()
	AddFastSymlink(t, fs, parentPath, name, target)
}

// inodeOffsetForPath computes the inode byte offset for the inode at the
// provided filesystem path. Used by package-local tests.
func inodeOffsetForPath(t *testing.T, r readerWriterAt, sb *superblock, path string) int64 {
	t.Helper()
	in, err := lookupPath(r, 0, sb, path)
	if err != nil {
		t.Fatalf("lookupPath(%q): %v", path, err)
	}
	return inodeOffsetFor(t, r, sb, in.num)
}

// dirBlockForPath returns the first data block for the given path.
func dirBlockForPath(t *testing.T, rw readerWriterAt, sb *superblock, path string) uint64 {
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

// corruptPathExtents zeroes the block/extent fields for the inode at path and
// marks it as using extents, then writes the inode back. This forces extent
// parsing to fail in certain error-path tests.
func corruptPathExtents(t *testing.T, rw readerWriterAt, sb *superblock, path string) {
	t.Helper()
	in, err := lookupPath(rw, 0, sb, path)
	if err != nil {
		t.Fatalf("lookupPath(%q): %v", path, err)
	}
	for i := range in.raw[inodeOffBlock : inodeOffBlock+60] {
		in.raw[inodeOffBlock+i] = 0
	}
	binary.LittleEndian.PutUint32(in.raw[inodeOffFlags:], InodeFlagExtents)
	if err := writeInode(rw, 0, sb, in); err != nil {
		t.Fatalf("writeInode(%q): %v", path, err)
	}
}
