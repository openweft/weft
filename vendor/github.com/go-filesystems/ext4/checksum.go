package filesystem_ext4

import "hash/crc32"

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c computes a running CRC-32C (Castagnoli) checksum.
// Pass the previous CRC value as crc (use 0 for the initial call, or the
// ext4 checksum seed when computing metadata checksums).
func crc32c(crc uint32, data []byte) uint32 {
	return crc32.Update(crc, castagnoli, data)
}
