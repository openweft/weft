// Package ext4 provides read/write access to ext4 filesystem images without
// requiring root privileges. It supports raw disk images (MBR/GPT) as well as
// bare filesystem images and handles modern ext4 features: extents, 64-bit
// block numbers, flex_bg, dir_htree and metadata_csum (CRC32c checksums).
package filesystem_ext4

// Incompatible feature flags (must understand to read/write).
const (
	FeatIncompatFiletype   = 0x0002
	FeatIncompatExtents    = 0x0040
	FeatIncompat64bit      = 0x0080
	FeatIncompatFlexBg     = 0x0200
	FeatIncompatCsumSeed   = 0x2000
	FeatIncompatInlineData = 0x8000
)

// Read-only compatible feature flags.
const (
	FeatROCompatSparseSuper  = 0x0001
	FeatROCompatLargeFile    = 0x0002
	FeatROCompatGdtCsum      = 0x0010
	FeatROCompatMetadataCsum = 0x0400
)

// Inode flags.
const (
	InodeFlagHashIndex  = 0x00001000 // directory uses htree index
	InodeFlagExtents    = 0x00080000 // inode uses extent tree
	InodeFlagInlineData = 0x10000000 // data stored inline in inode
)

// Extent tree magic number.
const ExtentMagic uint16 = 0xF30A

// Well-known inode numbers.
const RootIno uint32 = 2

// Directory entry file types.
const (
	FtUnknown = 0
	FtRegFile = 1
	FtDir     = 2
	FtSymlink = 7
	FtDirTail = 0xDE // fake tail entry carrying checksum
)

// DirEntry is a parsed directory entry.
type DirEntry struct {
	Inode    uint32
	Name     string
	FileType uint8
}
