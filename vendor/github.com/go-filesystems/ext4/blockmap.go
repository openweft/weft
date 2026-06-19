package filesystem_ext4

import (
	"encoding/binary"
	"fmt"
)

// blockMapResolver returns a closure mapping a logical block number to its
// physical block (0 = sparse hole) for an inode that uses the classic
// ext2/ext3 indirect block map (EXT4_EXTENTS_FL clear).
//
// The 60-byte i_block area holds 15 little-endian uint32 block pointers:
//
//	[0..11]  direct blocks
//	[12]     single-indirect block (holds BlockSize/4 pointers)
//	[13]     double-indirect block (pointers to single-indirect blocks)
//	[14]     triple-indirect block (pointers to double-indirect blocks)
//
// Indirect blocks are read through a small cache so resolving every logical
// block of a large file does not re-read the same indirect block repeatedly.
func (in *inode) blockMapResolver(f readerWriterAt, fsOffset int64, sb *superblock) (func(uint64) (uint64, error), int, error) {
	bs := int(sb.BlockSize)
	if bs <= 0 {
		return nil, 0, fmt.Errorf("ext4: blockMap: invalid block size %d", bs)
	}
	ptrs := uint64(bs / 4)
	le := binary.LittleEndian
	iblock := in.raw[inodeOffBlock : inodeOffBlock+60]

	cache := map[uint64][]byte{}
	ptrBlock := func(b uint64) ([]byte, error) {
		if b == 0 {
			return nil, nil // hole: entire subtree is zero
		}
		if c, ok := cache[b]; ok {
			return c, nil
		}
		buf, err := readRawBlock(f, fsOffset, sb, b)
		if err != nil {
			return nil, err
		}
		cache[b] = buf
		return buf, nil
	}
	entry := func(buf []byte, idx uint64) uint64 {
		if buf == nil {
			return 0
		}
		return uint64(le.Uint32(buf[idx*4:]))
	}

	resolve := func(lbn uint64) (uint64, error) {
		if lbn < 12 {
			return uint64(le.Uint32(iblock[lbn*4:])), nil
		}
		lbn -= 12
		if lbn < ptrs {
			b, err := ptrBlock(uint64(le.Uint32(iblock[12*4:])))
			if err != nil {
				return 0, err
			}
			return entry(b, lbn), nil
		}
		lbn -= ptrs
		if lbn < ptrs*ptrs {
			outer, err := ptrBlock(uint64(le.Uint32(iblock[13*4:])))
			if err != nil {
				return 0, err
			}
			mid, err := ptrBlock(entry(outer, lbn/ptrs))
			if err != nil {
				return 0, err
			}
			return entry(mid, lbn%ptrs), nil
		}
		lbn -= ptrs * ptrs
		top, err := ptrBlock(uint64(le.Uint32(iblock[14*4:])))
		if err != nil {
			return 0, err
		}
		outer, err := ptrBlock(entry(top, lbn/(ptrs*ptrs)))
		if err != nil {
			return 0, err
		}
		rem := lbn % (ptrs * ptrs)
		mid, err := ptrBlock(entry(outer, rem/ptrs))
		if err != nil {
			return 0, err
		}
		return entry(mid, rem%ptrs), nil
	}
	return resolve, bs, nil
}

// blockMapExtents synthesises an extent list equivalent to the file's indirect
// block map, coalescing logically- and physically-contiguous runs. Holes
// (pointer 0) are omitted, so the LogBlock of each leaf is meaningful. This
// lets directory traversal and other extent() consumers work transparently on
// ext2/ext3 inodes.
func (in *inode) blockMapExtents(f readerWriterAt, fsOffset int64, sb *superblock) ([]extentLeaf, error) {
	resolve, bs, err := in.blockMapResolver(f, fsOffset, sb)
	if err != nil {
		return nil, err
	}
	size := int(in.size)
	if size == 0 {
		return nil, nil
	}
	nblocks := (size + bs - 1) / bs
	var out []extentLeaf
	last := -1
	for i := 0; i < nblocks; i++ {
		phys, err := resolve(uint64(i))
		if err != nil {
			return nil, err
		}
		if phys == 0 {
			last = -1 // hole breaks the contiguous run
			continue
		}
		if last >= 0 &&
			out[last].Count < 0xFFFF &&
			out[last].PhysBlock+uint64(out[last].Count) == phys &&
			uint64(out[last].LogBlock)+uint64(out[last].Count) == uint64(i) {
			out[last].Count++
			continue
		}
		out = append(out, extentLeaf{LogBlock: uint32(i), PhysBlock: phys, Count: 1})
		last = len(out) - 1
	}
	return out, nil
}

// blockMapData reads the full content of a block-map file, honouring sparse
// holes (which read back as zeroes). The result is exactly in.size bytes.
func (in *inode) blockMapData(f readerWriterAt, fsOffset int64, sb *superblock) ([]byte, error) {
	size := int(in.size)
	out := make([]byte, size)
	if size == 0 {
		return out, nil
	}
	resolve, bs, err := in.blockMapResolver(f, fsOffset, sb)
	if err != nil {
		return nil, err
	}
	nblocks := (size + bs - 1) / bs
	for i := 0; i < nblocks; i++ {
		phys, err := resolve(uint64(i))
		if err != nil {
			return nil, err
		}
		if phys == 0 {
			continue // sparse hole; out is already zeroed
		}
		blk, err := readRawBlock(f, fsOffset, sb, phys)
		if err != nil {
			return nil, fmt.Errorf("ext4: read data block %d: %w", phys, err)
		}
		off := i * bs
		n := bs
		if off+n > size {
			n = size - off
		}
		copy(out[off:off+n], blk[:n])
	}
	return out, nil
}
