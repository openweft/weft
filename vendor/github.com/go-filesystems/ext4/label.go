package filesystem_ext4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"

	filesystem "github.com/go-filesystems/interface"
)

// MaxLabelLen is the on-disk size of the ext4 volume label
// (s_volume_name, at superblock offset 0x78).
const MaxLabelLen = 16

// Compile-time assertion: ext4FS implements filesystem.Labeller.
var _ filesystem.Labeller = (*ext4FS)(nil)

// Label returns the current volume label, decoded from s_volume_name.
// An empty string means the filesystem has no label set.
func (fs *ext4FS) Label() string {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.sb.Label
}

// SetLabel writes a new volume label into the superblock. The label
// must be at most MaxLabelLen bytes once UTF-8 encoded; shorter labels
// are null-padded to 16 bytes. When metadata_csum is enabled the
// superblock CRC32c is recomputed; the on-disk write touches only the
// 1024-byte superblock.
//
// Concurrency: SetLabel takes the FS write lock for the duration of
// the operation. It does NOT go through the journal — it's a direct
// WriteAt — which is the right call for offline-style relabelling.
// Calling SetLabel concurrently with active writers (other goroutines
// modifying the same filesystem) may produce a torn superblock; use
// only on a filesystem that no other writer is touching.
func (fs *ext4FS) SetLabel(label string) error {
	b := []byte(label)
	if len(b) > MaxLabelLen {
		return fmt.Errorf("ext4: label %q is %d bytes, exceeds maximum %d", label, len(b), MaxLabelLen)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Pull a fresh copy of the on-disk superblock so we don't fight
	// any stale fields the in-memory decode might be holding (free
	// counts get touched by Grow / allocations and we don't want our
	// label write to inadvertently roll those back).
	raw := make([]byte, superblockSize)
	if _, err := fs.f.ReadAt(raw, fs.partOffset+1024); err != nil {
		return fmt.Errorf("ext4: read superblock: %w", err)
	}
	le := binary.LittleEndian
	if le.Uint16(raw[56:]) != 0xEF53 {
		return fmt.Errorf("ext4: bad superblock magic when relabeling")
	}

	// Zero the field then copy — keeps the trailing bytes clean when
	// the new label is shorter than the previous one.
	for i := 0; i < MaxLabelLen; i++ {
		raw[120+i] = 0
	}
	copy(raw[120:], b)

	// Recompute the superblock CRC32c if metadata_csum is enabled.
	//
	// IMPORTANT: the superblock has its own checksum convention,
	// distinct from every other ext4 metadata block. It uses
	//   crc32c(~0, sb_bytes[:0x3FC])
	// — i.e. the kernel-canonical CRC32c (init = ~0, no final XOR),
	// independent of s_checksum_seed. The rest of ext4's metadata
	// (block group descriptors, inodes, dir entries …) DOES seed
	// from s_checksum_seed or crc32c(~0, uuid); the superblock is
	// the exception. See fs/ext4/super.c::ext4_superblock_csum() and
	// e2fsprogs lib/ext2fs/csum.c::ext2fs_superblock_csum_set().
	//
	// Go's hash/crc32 with a Castagnoli table does its own ^crc
	// at the start and ^result at the end (simpleUpdate), so to
	// match the kernel we have to invert what we'd naively pass:
	//   kernel crc32c(~0, data) == ^crc32.Update(0, castagnoli, data)
	// (Verifiable via the standard test vector "123456789" →
	// 0xE3069283 == crc32.Update(0, castagnoli, "123456789").)
	if le.Uint32(raw[100:])&FeatROCompatMetadataCsum != 0 {
		le.PutUint32(raw[0x3FC:], 0)
		csum := ^crc32.Update(0, castagnoli, raw[:0x3FC])
		le.PutUint32(raw[0x3FC:], csum)
	}

	if _, err := fs.f.WriteAt(raw, fs.partOffset+1024); err != nil {
		return fmt.Errorf("ext4: write superblock: %w", err)
	}
	if err := fs.f.Sync(); err != nil {
		return fmt.Errorf("ext4: sync superblock: %w", err)
	}

	// Keep the in-memory cache coherent with the on-disk truth.
	copy(fs.sb.raw[120:136], raw[120:136])
	copy(fs.sb.raw[0x3FC:0x400], raw[0x3FC:0x400])
	fs.sb.Label = decodeNullPadded(raw[120:136])
	return nil
}

// decodeNullPadded converts a null-padded byte slice to a Go string,
// dropping every trailing zero byte. Shared by readSuperblock and
// SetLabel so the two encode/decode in lockstep.
func decodeNullPadded(b []byte) string {
	return string(bytes.TrimRight(b, "\x00"))
}
