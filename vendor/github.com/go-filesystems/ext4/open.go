package filesystem_ext4

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/safeio"
)

// Verify implementation of the optional read-at-an-offset interface.
var _ filesystem.Opener = (*ext4FS)(nil)

// openFileReadInode is the inode re-read OpenFile performs under the inode
// lock. It is a package var, following readLinkReadInode above, so a test can
// drive the failure branch: the re-read and the preceding path lookup both hit
// the inode table, so a device-level fault cannot fail one without the other.
var openFileReadInode = readInode

// ext4File is an open regular file on an ext4 (or ext2/ext3) image, backing
// filesystem.File.
//
// ReadFile materialises the whole file, which is unusable for anything serving
// reads on demand: a 4 KiB request out of a 4 GiB file allocated 4 GiB. What
// this type materialises instead is the file's MAP — the extent leaves, each
// one a (logical block, physical block, block count) triple — which is orders
// of magnitude smaller and is metadata the driver already parses. No file data
// is read at open, with one deliberate exception: an inline-data inode, whose
// contents live inside the inode itself (i_block plus the system.data xattr)
// and are bounded by one block, so reading them at open is both cheap and the
// only way to serve them at all.
//
// Three on-disk layouts converge here. Extent-mapped inodes (ext4) and classic
// indirect block maps (ext2/ext3) both come back from inode.readExtents as the
// same leaf list — the same call ReadFile's readFileData path uses, so nothing
// here re-implements block resolution. Inline data is the third.
//
// Holes are the reason the map is searched rather than indexed: a sparse file's
// extents cover only the allocated regions, and the gaps read back as zeros,
// exactly as readFileData's pre-zeroed buffer produces them.
//
// The map is a SNAPSHOT taken at OpenFile, under the same inode lock ReadFile
// takes. A File therefore describes the file as it was when opened; a file
// rewritten through the same Filesystem afterwards must be reopened. ReadAt
// holds the filesystem's read lock so a read cannot interleave with a
// structural mutation, and because that lock is shared, concurrent ReadAt calls
// still proceed in parallel as io.ReaderAt requires.
type ext4File struct {
	fs *ext4FS
	// inline holds the whole file when the inode carries its data inline;
	// extents is nil in that case, and vice versa.
	inline []byte
	// extents is the file's block map, sorted by logical block. Holes are
	// the gaps between leaves.
	extents   []extentLeaf
	size      int64
	blockSize int64
	inodeNum  uint32
	closed    atomic.Bool
	// mu guards extents and size against a concurrent extend or truncate
	// through this File (see writeat.go). It is separate from the
	// filesystem's mu, which orders this File against structural changes
	// made elsewhere.
	mu sync.RWMutex
}

var _ filesystem.File = (*ext4File)(nil)

// OpenFile opens the regular file at path for random access.
//
// It resolves the path and snapshots the inode's block map, reading no file
// data (an inline-data inode excepted — see ext4File). The locking mirrors
// ReadFile exactly: the filesystem read lock is held only for the path lookup,
// then the per-inode lock is taken and the inode re-read under it, so the map
// is never derived from a partially-applied inode update.
func (fs *ext4FS) OpenFile(path string) (filesystem.File, error) {
	fs.mu.RLock()
	rw := getRW(fs)
	in, err := lookupPath(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		fs.mu.RUnlock()
		return nil, err
	}
	if !in.isRegular() {
		fs.mu.RUnlock()
		return nil, fmt.Errorf("ext4: %q is not a regular file", path)
	}
	ino := in.num
	fs.mu.RUnlock()

	owner := NewOwner()
	il := getInodeLock(rw, ino)
	il.LockOwner(owner)
	defer il.UnlockOwner(owner)

	freshIn, err := openFileReadInode(rw, fs.partOffset, fs.sb, ino)
	if err != nil {
		return nil, err
	}
	return fs.newFile(rw, freshIn)
}

// newFile builds the File for an already-locked, freshly-read inode. It is
// split out of OpenFile so the map-construction rules can be exercised against
// an inode obtained by other means.
func (fs *ext4FS) newFile(rw readerWriterAt, in *inode) (filesystem.File, error) {
	// Inline data is bounded by one block and lives in the inode, so it is
	// simply materialised: there is no block map to search.
	//
	// It is also the first of the two layouts this driver cannot write
	// positionally, so the File comes back wrapped as read-only — see
	// readOnlyFile in writeat.go for why the distinction is made on the File
	// rather than refused later from WriteAt.
	if in.isInline() {
		data, err := in.inlineData(rw, fs.partOffset, fs.sb)
		if err != nil {
			return nil, err
		}
		return &readOnlyFile{inner: &ext4File{fs: fs, inline: data, size: int64(len(data)), blockSize: int64(fs.sb.BlockSize), inodeNum: in.num}}, nil
	}

	ext, err := in.readExtents(rw, fs.partOffset, fs.sb)
	if err != nil {
		return nil, err
	}

	// H2: i_size is attacker-controlled. ReadFile bounds it by the whole
	// filesystem's byte size before allocating; this path allocates nothing,
	// but the same bound is still the honest definition of a corrupt inode —
	// no file can be larger than the filesystem holding it — and applying it
	// keeps OpenFile and ReadFile agreeing about which inodes are readable.
	// The ceiling must NOT be the bytes the extents supply: a sparse file's
	// i_size legitimately exceeds them.
	max := int64(fs.sb.BlocksCount) * int64(fs.sb.BlockSize)
	if max <= 0 {
		// Filesystem size unknown (BlocksCount == 0 — only synthetic
		// superblocks). Fall back to the highest logical byte the extents
		// can address, as readFileData does.
		for _, e := range ext {
			if end := (int64(e.LogBlock) + int64(e.Count)) * int64(fs.sb.BlockSize); end > max {
				max = end
			}
		}
	}
	if max < 0 || in.size > uint64(max) {
		return nil, fmt.Errorf("ext4: inode %d size %d invalid: %w", in.num, in.size, safeio.ErrTooLarge)
	}
	size := int64(in.size)

	// An extent-mapped inode claiming a size with no extents at all is
	// corrupt; readFileData refuses it rather than returning zeros. A
	// block-map inode is different — a fully sparse ext2/ext3 file really
	// does yield no leaves and really does read back as zeros — so the check
	// is scoped to extent-mapped inodes, exactly as readFileData scopes it.
	if size > 0 && len(ext) == 0 && in.flags()&InodeFlagExtents != 0 {
		return nil, fmt.Errorf("ext4: inode %d has size %d but no extents", in.num, in.size)
	}

	// The tree is walked in on-disk order, which is normally ascending by
	// logical block but is not guaranteed to be by a hostile image. Sorting
	// is what lets ReadAt binary-search, and it costs one sort per open.
	sorted := make([]extentLeaf, len(ext))
	copy(sorted, ext)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].LogBlock < sorted[j].LogBlock })

	file := &ext4File{
		fs:        fs,
		extents:   sorted,
		size:      size,
		blockSize: int64(fs.sb.BlockSize),
		inodeNum:  in.num,
	}
	// The classic ext2/ext3 indirect block map is the second layout this
	// driver cannot write positionally: growing one means allocating indirect
	// blocks, which no write path in this package has ever done (writeFile
	// only ever produces extent-mapped inodes, and freeInodeBlocks skips block
	// maps outright). Reading one is fully supported, so it comes back as a
	// plain, read-only File and the caller falls back — which is exactly what
	// the capability probe is for.
	if in.flags()&InodeFlagExtents == 0 {
		return &readOnlyFile{inner: file}, nil
	}
	return file, nil
}

// Size returns the file's length in bytes: i_size as read at OpenFile, then
// tracking every extend or truncate performed through this File. No I/O.
func (f *ext4File) Size() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.size
}

// Close releases the File. ext4 files hold no per-file handle — the image
// handle stays owned by the Filesystem — so Close only marks the File unusable,
// which turns a use-after-close into a clear os.ErrClosed instead of a silent
// read through a stale block map. It is idempotent.
func (f *ext4File) Close() error {
	f.closed.Store(true)
	return nil
}

// ReadAt implements io.ReaderAt to the letter, the contract io.SectionReader
// and every generic consumer silently depend on:
//
//   - p is filled completely with a nil error whenever the bytes exist;
//   - n < len(p) comes back only together with a non-nil error;
//   - a read running into the end of the file returns io.EOF with whatever
//     bytes it did get, and an offset at or past Size() returns 0, io.EOF.
//
// Each iteration locates the extent covering the current logical block by
// binary search, then reads the whole contiguous run it can — an extent is
// contiguous on disk, so one ReadAt can span many blocks — clipped to what the
// caller asked for and to i_size. An offset that falls in a hole (no extent
// covers it, or the file's map ends early) yields zeros without touching the
// device, which is what ReadFile's pre-zeroed buffer returns for the same
// bytes.
func (f *ext4File) ReadAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("ext4: ReadAt: negative offset %d", off)
	}
	// The File's own lock keeps a read from observing a half-applied extend
	// or truncate; it is shared, so parallel ReadAt calls stay parallel.
	f.mu.RLock()
	defer f.mu.RUnlock()
	if off >= f.size {
		return 0, io.EOF
	}
	if f.inline != nil {
		n := copy(p, f.inline[off:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}

	// The filesystem read lock keeps a read from interleaving with a
	// structural mutation; it is shared, so parallel ReadAt calls are not
	// serialised against each other.
	f.fs.mu.RLock()
	defer f.fs.mu.RUnlock()
	rw := getRW(f.fs)

	n := 0
	for n < len(p) {
		cur := off + int64(n)
		if cur >= f.size {
			return n, io.EOF
		}
		want := int64(len(p) - n)
		if rem := f.size - cur; want > rem {
			want = rem
		}
		logBlock := uint64(cur / f.blockSize)
		within := cur % f.blockSize

		// First extent whose logical range ends after this block.
		i := sort.Search(len(f.extents), func(i int) bool {
			e := f.extents[i]
			return uint64(e.LogBlock)+uint64(e.Count) > logBlock
		})
		if i == len(f.extents) || uint64(f.extents[i].LogBlock) > logBlock {
			// Hole: no extent maps this block. Zero-fill up to the start of
			// the next extent (or to the end of the read), never past it.
			chunk := want
			if i < len(f.extents) {
				holeEnd := int64(f.extents[i].LogBlock) * f.blockSize
				if lim := holeEnd - cur; lim < chunk {
					chunk = lim
				}
			}
			for j := int64(0); j < chunk; j++ {
				p[n+int(j)] = 0
			}
			n += int(chunk)
			continue
		}

		e := f.extents[i]
		skip := logBlock - uint64(e.LogBlock)
		physBlock := e.PhysBlock + skip
		// Bytes left in this extent from the current position.
		avail := (int64(e.Count)-int64(skip))*f.blockSize - within
		chunk := want
		if avail < chunk {
			chunk = avail
		}
		// M3: every physical block touched must lie inside the filesystem
		// before an offset is computed from it. Checking the last block of
		// the run covers the whole run, the blocks being consecutive.
		if f.fs.sb.BlocksCount != 0 {
			lastBlock := physBlock + uint64((within+chunk-1)/f.blockSize)
			if lastBlock >= f.fs.sb.BlocksCount {
				return n, fmt.Errorf("ext4: data block %d out of range (blocks=%d) in inode %d", lastBlock, f.fs.sb.BlocksCount, f.inodeNum)
			}
		}
		diskOff := f.fs.partOffset + int64(physBlock)*f.blockSize + within
		m, err := rw.ReadAt(p[n:n+int(chunk)], diskOff)
		n += m
		if err != nil {
			return n, fmt.Errorf("ext4: read data block %d: %w", physBlock, err)
		}
	}
	return n, nil
}
