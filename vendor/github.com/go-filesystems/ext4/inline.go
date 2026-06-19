package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
)

// ext4 inline_data layout
// ------------------------
// When EXT4_INLINE_DATA_FL (InodeFlagInlineData) is set the file/directory
// data is stored inside the inode rather than in data blocks:
//
//   - The first up to 60 bytes live in the 60-byte i_block area
//     (offset inodeOffBlock).
//   - If the data is larger than 60 bytes the remainder is stored in the
//     extended attribute "system.data" (name_index = 7, name = "data").
//     That attribute lives in the in-inode xattr area (the space between
//     128 + i_extra_isize and the end of the inode) and/or, when it does not
//     fit there, in the external xattr block referenced by i_file_acl.
//
// The on-disk byte order of the inline content is: i_block bytes first, then
// the bytes from the system.data attribute value appended after them.
//
// For directories the same layout applies, except logical block 0 of a normal
// directory (the "." / ".." block plus following entries) is split: the "."
// and ".." entries are synthesised from a small fixed header at the start of
// i_block and the regular entries follow. We expose the raw inline bytes and
// let the directory parser interpret them the same way it parses a directory
// data block.

const (
	// xattrInodeMagic is the magic at the start of the in-inode xattr area
	// (stored little-endian).
	xattrInodeMagic uint32 = 0xEA020000
	// xattrBlockMagic is the magic at the start of an external xattr block.
	xattrBlockMagic uint32 = 0xEA020000

	// xattrIndexSystem is the e_name_index value used for the "system."
	// namespace, under which the "data" attribute lives ("system.data").
	xattrIndexSystem uint8 = 7

	// inlineIBlockMax is the number of data bytes that fit in the i_block area.
	inlineIBlockMax = 60

	// xattrEntrySize is the fixed-size header of an xattr entry (excluding
	// the variable-length name that follows).
	xattrEntrySize = 16
)

// inodeExtraIsize returns the i_extra_isize value for the inode, clamped to a
// sane range. For inodes of size 128 (no extra space) this is 0.
func (in *inode) inodeExtraIsize() int {
	if len(in.raw) <= inodeOffExtraIsize+1 {
		return 0
	}
	extra := int(binary.LittleEndian.Uint16(in.raw[inodeOffExtraIsize:]))
	if extra < 0 {
		return 0
	}
	// The in-inode xattr area starts at 128 + i_extra_isize; clamp so it never
	// runs past the end of the inode buffer.
	if 128+extra > len(in.raw) {
		return len(in.raw) - 128
	}
	return extra
}

// isInline reports whether the inode stores its data inline.
func (in *inode) isInline() bool {
	return in.flags()&InodeFlagInlineData != 0
}

// findSystemDataXattr searches an xattr entry table for the "system.data"
// attribute and returns its value bytes. entriesStart is the offset (within
// buf) of the first entry; valueBase is the offset that e_value_offs values
// are measured from. It returns (value, found, error). A malformed table is
// treated as "not found" rather than an error so callers can fall back to the
// i_block-only content, matching the kernel's tolerance for short inodes.
func findSystemDataXattr(buf []byte, entriesStart, valueBase int) ([]byte, bool, error) {
	le := binary.LittleEndian
	off := entriesStart
	for off+4 <= len(buf) {
		// A zero entry header (e_name_len == 0 && e_name_index == 0) marks the
		// end of the entry list.
		nameLen := int(buf[off])
		nameIndex := buf[off+1]
		if nameLen == 0 && nameIndex == 0 {
			break
		}
		if off+xattrEntrySize > len(buf) {
			return nil, false, nil
		}
		valueOffs := int(le.Uint16(buf[off+2:]))
		valueInum := le.Uint32(buf[off+4:])
		valueSize := int(le.Uint32(buf[off+8:]))
		nameOff := off + xattrEntrySize
		if nameOff+nameLen > len(buf) {
			return nil, false, nil
		}
		name := string(buf[nameOff : nameOff+nameLen])

		if nameIndex == xattrIndexSystem && name == "data" {
			if valueInum != 0 {
				// Value stored in a separate inode (EA-inode feature). Reading
				// that is out of scope for inline_data; report it explicitly.
				return nil, false, fmt.Errorf("ext4: system.data xattr stored in EA inode %d (unsupported)", valueInum)
			}
			vstart := valueBase + valueOffs
			if vstart < 0 || valueSize < 0 || vstart+valueSize > len(buf) {
				return nil, false, fmt.Errorf("ext4: system.data xattr value out of range (offs=%d size=%d)", valueOffs, valueSize)
			}
			return buf[vstart : vstart+valueSize], true, nil
		}
		// Advance to the next entry: header + name, padded to 4 bytes.
		off = nameOff + ((nameLen + 3) &^ 3)
	}
	return nil, false, nil
}

// systemDataValue returns the bytes of the "system.data" extended attribute,
// searching the in-inode xattr area first and then the external xattr block
// referenced by i_file_acl. It returns (value, found, error).
func (in *inode) systemDataValue(f readerWriterAt, fsOffset int64, sb *superblock) ([]byte, bool, error) {
	le := binary.LittleEndian

	// In-inode xattr area: starts at 128 + i_extra_isize, prefixed by a
	// 4-byte magic, followed by the entry table. e_value_offs are measured
	// from the byte right after the magic (i.e. the start of the entries).
	extra := in.inodeExtraIsize()
	hdr := 128 + extra
	if hdr+4 <= len(in.raw) {
		if le.Uint32(in.raw[hdr:]) == xattrInodeMagic {
			entriesStart := hdr + 4
			val, found, err := findSystemDataXattr(in.raw, entriesStart, entriesStart)
			if err != nil {
				return nil, false, err
			}
			if found {
				return val, true, nil
			}
		}
	}

	// External xattr block referenced by i_file_acl.
	if len(in.raw) >= inodeOffFilACLLo+4 {
		aclBlock := uint64(le.Uint32(in.raw[inodeOffFilACLLo:]))
		if aclBlock != 0 {
			buf, err := readRawBlock(f, fsOffset, sb, aclBlock)
			if err != nil {
				return nil, false, fmt.Errorf("ext4: read xattr block %d: %w", aclBlock, err)
			}
			// External block: 32-byte header beginning with the magic, then the
			// entry table. e_value_offs are measured from the start of the block.
			const xattrBlockHeaderSize = 32
			if len(buf) >= 4 && le.Uint32(buf[0:]) == xattrBlockMagic {
				return findSystemDataXattr(buf, xattrBlockHeaderSize, 0)
			}
		}
	}

	return nil, false, nil
}

// inlineData assembles and returns the full inline content of the inode: the
// i_block-resident prefix (up to 60 bytes) followed by the system.data xattr
// value when the data overflows i_block. The result is exactly in.size bytes.
func (in *inode) inlineData(f readerWriterAt, fsOffset int64, sb *superblock) ([]byte, error) {
	size := int(in.size)
	if size < 0 {
		return nil, fmt.Errorf("ext4: inode %d has negative size", in.num)
	}

	iblock := in.raw[inodeOffBlock : inodeOffBlock+inlineIBlockMax]

	// Small case: data fits entirely in i_block.
	if size <= inlineIBlockMax {
		out := make([]byte, size)
		copy(out, iblock[:size])
		return out, nil
	}

	// Overflow case: remaining bytes live in system.data.
	val, found, err := in.systemDataValue(f, fsOffset, sb)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("ext4: inode %d size %d exceeds i_block but system.data xattr is missing", in.num, size)
	}
	overflow := size - inlineIBlockMax
	if overflow > len(val) {
		return nil, fmt.Errorf("ext4: inode %d inline overflow %d exceeds system.data value %d", in.num, overflow, len(val))
	}
	out := make([]byte, 0, size)
	out = append(out, iblock...)
	out = append(out, val[:overflow]...)
	return out, nil
}

// inlineDirEntries parses the directory entries stored inline in the inode.
//
// For inline directories the on-disk layout differs from a normal directory
// block (this matches the Linux kernel's ext4_inline_data layout):
//
//   - The first 4 bytes of i_block hold the inode number of the PARENT, i.e.
//     the ".." entry. Neither "." nor ".." is stored as a real dirent record.
//   - The named-entry dirent stream (regular ext4_dir_entry_2 records) starts
//     at offset 4 of i_block.
//   - The "." entry is the inode's OWN number (in.num) and is never stored on
//     disk; it is synthesised here.
//
// So we synthesise "." (in.num) and ".." (u32 at i_block[0:4]), then parse the
// named stream from offset 4. Any overflow lives in the system.data xattr and
// is a plain directory-entry stream of named entries (no 4-byte prefix).
func inlineDirEntries(f readerWriterAt, fsOffset int64, sb *superblock, in *inode) ([]DirEntry, error) {
	le := binary.LittleEndian
	iblock := in.raw[inodeOffBlock : inodeOffBlock+inlineIBlockMax]

	var entries []DirEntry

	// "." entry: implicit, its inode number is the inode itself (in.num) and is
	// not stored on disk. Synthesise it.
	entries = append(entries, DirEntry{Inode: in.num, Name: ".", FileType: FtDir})

	// ".." entry: implicit, its inode number is the parent, stored in the first
	// 4 bytes of i_block. Synthesise it.
	parentIno := le.Uint32(iblock[0:])
	entries = append(entries, DirEntry{Inode: parentIno, Name: "..", FileType: FtDir})

	// The named-entry stream begins at offset 4 of i_block.
	entries = append(entries, parseDirBlock(iblock[4:])...)

	// Overflow region in system.data, if the directory is larger than 60 bytes.
	if int(in.size) > inlineIBlockMax {
		val, found, err := in.systemDataValue(f, fsOffset, sb)
		if err != nil {
			return nil, err
		}
		if found {
			entries = append(entries, parseDirBlock(val)...)
		}
	}

	return entries, nil
}
