package filesystem

import (
	"os"
	"time"
)

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
