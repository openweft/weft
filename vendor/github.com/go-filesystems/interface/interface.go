package filesystem

import "os"

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

// Labeller is the optional interface implemented by filesystems that
// expose a volume label. Callers probe via a type assertion:
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
	// Label returns the current volume label, decoded from the
	// implementation's on-disk metadata. An empty string means the
	// filesystem has no label set (not an error).
	Label() string
	// SetLabel writes a new volume label. Concrete implementations
	// document whether the call is safe with a live, actively-mutating
	// filesystem; the conservative assumption is "offline only".
	SetLabel(label string) error
}
