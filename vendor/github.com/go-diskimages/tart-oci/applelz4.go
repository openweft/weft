package tartoci

import (
	"encoding/binary"

	"github.com/go-compressions/lz4"
)

// appleBlockSize is the uncompressed chunk size Apple's Compression framework
// uses for each LZ4 frame block (64 KiB). encodeAppleFrame follows it so the
// frames it emits match what Tart/macOS produce.
const appleBlockSize = 1 << 16

// encodeAppleFrame encodes raw as an Apple LZ4 frame (a sequence of "bv41"
// compressed blocks followed by the "bv4$" end marker) that
// github.com/go-compressions/lz4 — and Apple's own decoder — can decode. Each
// block is an independent LZ4 block, which is a valid (if not maximally dense)
// member of the frame family.
//
// It is used to build local Tart OCI layouts (see Format.Create) and is the
// inverse of lz4.DecompressAppleStream for the images this package writes.
func encodeAppleFrame(raw []byte) []byte {
	out := make([]byte, 0, len(raw)/2+16)
	for off := 0; off < len(raw); off += appleBlockSize {
		end := off + appleBlockSize
		if end > len(raw) {
			end = len(raw)
		}
		chunk := raw[off:end]
		comp := lz4.CompressBlock(chunk)
		out = append(out, 'b', 'v', '4', '1')
		var hdr [8]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(chunk)))
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(comp)))
		out = append(out, hdr[:]...)
		out = append(out, comp...)
	}
	return append(out, 'b', 'v', '4', '$')
}
