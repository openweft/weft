package filesystem

import (
	"errors"
	"io"
	"os"
	"time"
)

// ErrShrinkUnsupported is returned by Resizer.Resize when the driver
// cannot shrink its on-disk format and the caller passed a newSize
// smaller than the current size. Filesystems whose layout precludes
// shrink entirely (e.g. XFS, ZFS) return this sentinel rather than
// attempting a destructive resize.
var ErrShrinkUnsupported = errors.New("filesystem: shrink not supported")

// DirEntry describes a directory entry. Implementations must provide accessors
// for the inode number, name and file type.
type DirEntry interface {
	Inode() uint64
	Name() string
	FileType() uint8
}

type dirEntry struct {
	inode    uint64
	name     string
	fileType uint8
}

func (d *dirEntry) Inode() uint64   { return d.inode }
func (d *dirEntry) Name() string    { return d.name }
func (d *dirEntry) FileType() uint8 { return d.fileType }

// NewDirEntry constructs a DirEntry implementation backed by an unexported
// struct. Returning the interface enforces encapsulation.
func NewDirEntry(inode uint64, name string, fileType uint8) DirEntry {
	return &dirEntry{inode: inode, name: name, fileType: fileType}
}

// Stat describes basic metadata for a filesystem path.
type Stat interface {
	Mode() uint16
	Size() uint64
	Inode() uint64
}

type stat struct {
	mode  uint16
	size  uint64
	inode uint64
}

func (s *stat) Mode() uint16  { return s.mode }
func (s *stat) Size() uint64  { return s.size }
func (s *stat) Inode() uint64 { return s.inode }

// NewStat constructs a Stat implementation backed by an unexported struct.
func NewStat(mode uint16, size uint64, inode uint64) Stat {
	return &stat{mode: mode, size: size, inode: inode}
}

// Filesystem defines a minimal common API implemented by concrete
// filesystem packages (ext4, xfs, btrfs).
//
// # ⛔⛔ THE ERROR CONTRACT, WHICH THIS INTERFACE NEVER STATED
//
// A path that is not there must return an error satisfying
// errors.Is(err, [io/fs.ErrNotExist]). Permission must satisfy
// fs.ErrPermission, and a path that already exists where one must not,
// fs.ErrExist.
//
// This was never written down, so it did not happen. Measured 2026-09-23
// across the family: TWELVE of fourteen drivers returned a bare fmt.Errorf --
// fat32 answered `fat32: "/x" not found`, which errors.Is cannot classify.
// Only apfs returned os.ErrNotExist.
//
// Re-measured 2026-09-24: all fourteen satisfy it. ffs is a re-export of ufs
// and has no error sites of its own. Each driver carries an
// errnotexist_test.go asserting the contract on its own operations, so the
// next driver that stops satisfying it fails in its own repository rather
// than at a server three modules away.
//
// # ⛔⛔ HOW A DRIVER SATISFIES IT MATTERS MORE THAN THAT IT DOES
//
// The last four took a week longer than the other ten, and not for want of
// effort: wrapping them the obvious way would have been WORSE than leaving
// them alone, and the reason generalises to whatever driver comes next.
//
// btrfs and xfs each raise ONE sentinel from two unrelated kinds of failure:
//
//   - a name that is not in a directory                    -> 404
//   - a B-tree descent, or a directory format, that fails  -> the image is
//     corrupt
//
// Wrapping the sentinel satisfies this contract and makes every server in
// this family answer "not found" for a broken filesystem. The fault then
// reaches a user as a routine 404 and nothing anywhere reports it. So in both
// drivers the marking is placed at the PATH BOUNDARY, on the error rather
// than the call site, and each carries a second test asserting that a
// structural failure is NOT fs.ErrNotExist.
//
// zfs and uefi needed one line each, because neither had ever overloaded its
// sentinel: zfs raises errNotFound at twenty-six sites and all twenty-six are
// a named thing that is not there, while a damaged ZAP returns its own
// unclassified error; uefi's is a linear scan of a slice, and a store that
// cannot be parsed fails earlier, in Open.
//
// The rule, for the next driver: BEFORE wrapping a sentinel, find every place
// that raises it. If any of them means "this image is not what it claims to
// be", the sentinel is the wrong place and the path boundary is the right
// one.
//
// That is not a tidiness question, but it is also not the one this paragraph
// first claimed. ⚠ AN EARLIER VERSION OF THIS COMMENT SAID WEBDAV ANSWERS 500
// FOR A MISSING FILE. IT DOES NOT, AND THE CORRECTION IS THE POINT.
//
// Measured, before and after wrapping fat32: GET of a missing file answered
// 404 both times. webdav's statusFor tries errors.Is FIRST and then falls back
// to a table of twelve message fragments -- "not found", "no such", "does not
// exist", and nine more -- so it catches today's drivers by their phrasing.
// The claim came from reading the errors.Is switch and stopping before the
// fallback underneath it.
//
// The real cost is what that fallback IS. go-filesystems/nfs carries the same
// twelve-entry table, copied, and webdav's own comment on it says:
//
//	this is a *last* resort ... The real fix belongs upstream: sentinel
//	errors in the interface module that every driver wraps.
//
// So correctness currently depends on how each driver PHRASES its errors. A
// driver that says "cannot locate" rather than "not found" gets the fallback
// status silently, with no test failing anywhere, and the same list has to be
// kept true in two repositories at once.
//
// go-filesystems/s3 had no such table and got InternalError for a missing key,
// which is what surfaced this at all.
//
// Wrapping is enough, and keeps the driver's own message:
//
//	return nil, fmt.Errorf("fat32: %q not found: %w", path, fs.ErrNotExist)
//
// A driver that cannot tell the cases apart should return the closest one it
// can justify rather than none: a caller can act on fs.ErrNotExist and cannot
// act on an unclassifiable string.
type Filesystem interface {
	Close() error
	ReadFile(path string) ([]byte, error)
	ListDir(path string) ([]DirEntry, error)
	Stat(path string) (Stat, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	ReadLink(path string) (string, error)
	MkDir(path string, perm os.FileMode) error
	DeleteFile(path string) error
	DeleteDir(path string) error
	Rename(oldPath, newPath string) error
}

// LabelReader is the optional read-only interface for filesystems that
// can decode an on-disk volume label. Implementations that can also
// mutate the label additionally satisfy Labeller (which embeds this).
//
// Some drivers (notably ones with transactional / COW write models)
// can read the label cheaply but cannot yet rewrite it through their
// regular commit machinery. Exposing the read capability separately
// lets generic tools (`diskimage exec <fs> label get`, status displays)
// work everywhere while keeping the write surface honest.
//
//	if r, ok := fs.(filesystem.LabelReader); ok {
//	    fmt.Println(r.Label())
//	}
type LabelReader interface {
	// Label returns the current volume label, decoded from the
	// implementation's on-disk metadata. An empty string means the
	// filesystem has no label set (not an error).
	Label() string
}

// Labeller is the optional interface implemented by filesystems that
// expose a read/write volume label. Embeds LabelReader so every
// Labeller is automatically a LabelReader too — generic code can
// downgrade the assertion when only read access is needed.
//
//	if l, ok := fs.(filesystem.Labeller); ok {
//	    l.SetLabel("rootfs")
//	}
//
// Kept separate from Filesystem so implementations that genuinely have
// no concept of a label (or where label mutation is non-trivial)
// aren't forced to stub it. The label's encoding and length limit are
// filesystem-specific (e.g. ext2/3/4 caps at 16 bytes; FAT caps at 11).
// SetLabel must reject labels exceeding its filesystem's limit.
type Labeller interface {
	LabelReader
	// SetLabel writes a new volume label. Concrete implementations
	// document whether the call is safe with a live, actively-mutating
	// filesystem; the conservative assumption is "offline only".
	SetLabel(label string) error
}

// Symlinker is the optional interface implemented by filesystems that
// support creating symbolic links. ReadLink is already part of
// Filesystem; this capability gates the write side.
//
//	if s, ok := fs.(filesystem.Symlinker); ok {
//	    s.Symlink("/target", "/link")
//	}
type Symlinker interface {
	// Symlink creates a symbolic link at linkPath whose target is the
	// literal string `target`. The parent of linkPath must exist; the
	// path itself must not. Symlink targets are stored as-is and are
	// not resolved at creation time.
	Symlink(target, linkPath string) error
}

// HardLinker is the optional interface for filesystems that support
// POSIX hardlinks (multiple directory entries pointing at the same
// inode). Directories cannot be hardlinked — implementations must
// reject that case.
type HardLinker interface {
	// Link adds a new directory entry at newPath that points at the
	// same inode as oldPath. The source must not be a directory and
	// newPath must not already exist. The source inode's nlink count
	// is bumped.
	Link(oldPath, newPath string) error
}

// Truncater is the optional interface for filesystems that support
// resizing a regular file in place. Growing extends the file with
// implicit zero-fill (sparse where the format allows); shrinking
// drops or trims the trailing data.
type Truncater interface {
	// Truncate resizes the regular file at path to newSize bytes.
	// mtime and ctime are refreshed per POSIX truncate(2).
	Truncate(path string, newSize int64) error
}

// MetadataSetter is the optional interface bundling the POSIX
// metadata mutators (chmod / chown / utimes). Filesystems that
// support any of these typically support all three, so they're
// bundled — drivers that only implement a subset can still expose
// the methods they have and return an error from the others. (The
// type assertion only proves they all compile, not that any one
// call must succeed.)
type MetadataSetter interface {
	// Chmod replaces the permission bits at path, preserving the
	// file-type bits. ctime is refreshed.
	Chmod(path string, perm os.FileMode) error
	// Chown updates uid/gid at path. ctime is refreshed; mode, body,
	// and the other timestamps are left alone.
	Chown(path string, uid, gid uint32) error
	// Chtimes sets atime and mtime at path. ctime is refreshed to
	// "now" per POSIX. Birth time (if the filesystem records one) is
	// left untouched.
	Chtimes(path string, atime, mtime time.Time) error
}

// Grower is the optional interface for filesystems that can expand
// in place to fill a larger backing image. The opposite direction
// (shrink) is intentionally not part of this surface — most
// filesystems' shrink paths are either unsupported or far more
// invasive than grow.
type Grower interface {
	// GrowTo resizes the underlying image and the filesystem
	// metadata so that it spans newSizeBytes. The implementation may
	// require that the new size is strictly larger than the current
	// size and/or aligned to a filesystem-specific boundary.
	GrowTo(newSizeBytes int64) error
}

// Resizer is the optional, uniform resize capability implemented by
// every driver that can change its on-disk size, in either direction.
// It supersedes the older grow-only Grower split by giving callers a
// single entry point and a clearly-typed sentinel for the one case
// where the semantics genuinely diverge (shrink-unsupported formats).
//
//	if r, ok := fs.(filesystem.Resizer); ok {
//	    if err := r.Resize(newSize); errors.Is(err, filesystem.ErrShrinkUnsupported) {
//	        // driver only grows — caller decides how to handle
//	    }
//	}
type Resizer interface {
	// Resize changes the filesystem's underlying size in bytes. The newSize
	// is the new total capacity of the on-disk image. Implementations may
	// reject sizes that would lose data; semantics per driver:
	//
	//   - grow (always allowed where the on-disk format supports it)
	//   - shrink (allowed only if the data fits in newSize; some FS forbid
	//     shrink entirely — e.g. XFS, ZFS — and return ErrShrinkUnsupported)
	//
	// Returns ErrShrinkUnsupported when the driver doesn't implement shrink
	// and newSize < current; returns wrapping I/O errors for failed grow.
	Resize(newSize int64) error
}

// Opener is the optional interface a filesystem implements when it can read
// part of a file without materialising the whole thing.
//
// The base Filesystem API is per-path: ReadFile(path) returns the ENTIRE file.
// That is fine for a config file and useless for anything that has to serve
// reads on demand — a mount, an NFS or 9P export, a loop-mounted image — where
// answering a 4 KiB request out of a 4 GiB file must not allocate 4 GiB. Opener
// is the capability that makes those callers possible; it is the single missing
// piece, not an OS-specific one, so it lives here rather than in any one driver.
//
// It is optional and non-breaking by construction: Filesystem is unchanged, and
// a caller probes for it and falls back when a driver does not have it.
//
//	if o, ok := fs.(filesystem.Opener); ok {
//	    f, err := o.OpenFile(path)
//	    if err != nil {
//	        return err
//	    }
//	    defer f.Close()
//	    n, err := f.ReadAt(buf, off)
//	    ...
//	}
//	// otherwise: data, err := fs.ReadFile(path) and slice it.
//
// Drivers whose on-disk model cannot answer a byte range without decoding
// everything before it (whole-file compression, for instance) should simply not
// implement Opener rather than emulate it with a hidden full read — the point of
// the probe is that the caller learns the truth and can budget accordingly.
type Opener interface {
	// OpenFile opens the regular file at path for random access. The
	// returned File holds whatever per-file state the driver needs (an
	// extent list, a cluster chain, a block map); it must be closed by the
	// caller. Opening must not read the file's contents.
	//
	// Implementations reject directories, and paths that do not resolve,
	// with the same errors their ReadFile would return. The returned File
	// remains valid only while the Filesystem is open: closing the
	// Filesystem invalidates every File obtained from it.
	OpenFile(path string) (File, error)
}

// File is an open file: random access, and the size the caller needs to
// answer a stat without reading anything.
//
// ReadAt follows io.ReaderAt TO THE LETTER, and that is the whole point of the
// type. Callers — io.SectionReader, io.NewSectionReader, bufio over one, every
// generic Go consumer — assume the documented contract:
//
//   - ReadAt returns n < len(p) ONLY together with a non-nil error explaining
//     why. A short read with a nil error is the one behaviour that breaks those
//     consumers silently, because they treat it as "keep going" and lose data.
//   - Reading that stops at end of file returns io.EOF, either with the last
//     bytes (n > 0) or on its own (n == 0). An offset at or past Size() returns
//     0, io.EOF.
//   - ReadAt does not affect, and is not affected by, any notion of a seek
//     offset; each call is independent.
//   - Multiple ReadAt calls on the same File may run concurrently. Read paths
//     must therefore not mutate shared state without synchronisation. This too
//     is io.ReaderAt's contract, and a mount serving parallel requests relies
//     on it.
//
// Size returns the file's length in bytes, taken from the metadata the driver
// already read when the File was opened, so a caller can answer a stat or size
// an io.SectionReader without touching the data.
type File interface {
	io.ReaderAt
	io.Closer

	// Size returns the length of the file in bytes, as recorded in the
	// filesystem's own metadata. It is fixed for the lifetime of the File:
	// a File is a snapshot of the file as it was when opened.
	Size() int64
}

// WritableFile is the optional upgrade of a File returned by OpenFile: a
// driver that can write in place returns one, and a caller assertion-tests for
// it exactly as it does for Opener.
//
// # Why it exists
//
// Filesystem's only write is WriteFile(path, data, perm): it replaces the
// WHOLE file. There is no positional write, so a caller that receives a write
// at a non-zero offset — an NFS or 9P export, a WebDAV or SFTP server, a
// loop-mounted image — has to read the entire file, splice the new bytes in,
// and write the whole thing back. That is O(filesize) per request, so a client
// streaming a file in fixed-size blocks costs O(n²) in total. This is not a
// theoretical cost: a 2 MiB sequential write over a real NFS mount, in 64 KiB
// blocks, took 23 seconds — 90 kB/s — and a soft-mounted client gave up with
// EIO partway through, because one round-trip exceeded its timeout.
//
// WritableFile is the write-side twin of Opener and closes that gap the same
// way: File already carries the per-file state the driver resolved at open
// time (an extent list, a cluster chain, a block map), and a positional write
// reuses it instead of rebuilding the file from its path on every call.
//
// # Optional and non-breaking
//
// Filesystem, Opener and File are unchanged. A driver that cannot write in
// place — a read-only format such as iso9660 or squashfs, or one whose write
// path is not yet positional — simply does not return a WritableFile, and the
// caller falls back to the read-modify-write it already has:
//
//	f, err := o.OpenFile(path)
//	if err != nil {
//	    return err
//	}
//	defer f.Close()
//	if w, ok := f.(filesystem.WritableFile); ok {
//	    if _, err := w.WriteAt(p, off); err != nil {
//	        return err
//	    }
//	    return w.Sync()
//	}
//	// otherwise: ReadFile, splice, WriteFile — correct, and quadratic.
//
// The probe is on the FILE, not on the Filesystem, because writability is a
// property of the opened object: a driver may open a regular file writably and
// still refuse — with a plain File — a path it cannot write positionally.
//
// # Size, and how it differs from a read-only File
//
// File documents Size as fixed for the File's lifetime, because a read-only
// File is a snapshot. A WritableFile is not a snapshot: it is a handle the
// caller mutates. Size therefore reflects this handle's own writes — after a
// WriteAt that extends the file, or a Truncate, Size returns the new length,
// with no I/O and no reopen. It still says nothing about changes made through
// another handle or through Filesystem's path-based calls; a File never
// promised to observe those.
//
// # Concurrency
//
// The same terms as io.ReaderAt and io.WriterAt, no more and no less:
// concurrent ReadAt calls are safe, and concurrent WriteAt calls are safe
// PROVIDED their byte ranges do not overlap — that is io.WriterAt's own
// wording, and callers must respect it. Truncate and Sync change or observe
// the whole file, so they are not safe against a concurrent WriteAt; a caller
// that issues them serialises against its own writes.
type WritableFile interface {
	File
	io.WriterAt

	// Truncate resizes the file to size bytes. Growing extends it with
	// zeros — sparsely where the format allows, so growing must not be
	// assumed to allocate. Shrinking drops the trailing bytes and releases
	// what the format lets the driver release. Size reports the new length
	// once Truncate returns nil.
	//
	// A negative size is an error. This is the file-scoped twin of the
	// path-scoped Truncater capability on Filesystem; they do not collide,
	// having different receivers, and a driver may well implement both.
	Truncate(size int64) error

	// Sync blocks until every byte written through this File has reached the
	// filesystem's backing store, and returns the error if it could not.
	//
	// It exists because a network server has to be able to answer "it is on
	// the medium" as a distinct, later event from "I accepted the write":
	// NFSv3 COMMIT (RFC 1813 §3.21) and SFTP's fsync@openssh.com extension
	// both require exactly that answer, and neither can be given honestly by
	// a driver that only ever buffers. A server that cannot commit must
	// report unstable writes instead of claiming stability it has not got.
	//
	// What "backing store" means is the driver's to state, and each one must
	// state it: for an image-backed driver it is the io.WriterAt it was
	// handed, which may itself be an *os.File whose own Sync the driver
	// calls, or a buffer in memory for which Sync is a no-op that returns
	// nil. Returning nil from a Sync that guarantees nothing is a lie the
	// caller cannot detect, so a driver in that position documents it.
	Sync() error
}
