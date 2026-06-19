//go:build !amd64 && !arm64 && !riscv64 && !loong64

package matchlen

import (
	"encoding/binary"
	"math/bits"
)

// bulkMatch is the portable fallback: compare 8 bytes at a time. Reading the
// words little-endian makes TrailingZeros find the lowest-index differing byte
// regardless of host endianness.
func bulkMatch(a, b []byte, limit int) int {
	i := 0
	for i+8 <= limit {
		if d := binary.LittleEndian.Uint64(a[i:]) ^ binary.LittleEndian.Uint64(b[i:]); d != 0 {
			return i + bits.TrailingZeros64(d)>>3
		}
		i += 8
	}
	return i
}
