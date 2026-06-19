package filesystem_ext4

import "hash/crc32"

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c computes the ext4/kernel flavour of a running CRC-32C (Castagnoli)
// checksum over data, continuing from the running value crc.
//
// IMPORTANT: ext4 (and e2fsprogs) compute metadata checksums with the
// "raw" reflected CRC32c — that is, the bit-reflected Castagnoli update
// WITHOUT the leading/trailing one-complement that Go's hash/crc32 applies
// internally. The kernel/e2fsprogs primitive is:
//
//	crc32c(seed, data) // init = seed, no pre/post inversion
//
// Go's crc32.Update(crc, castagnoli, data) instead computes the canonical
// CRC32c which is equivalent to ^crc32.Update(^crc, ...) of the raw form.
// Inverting on the way in and out recovers the kernel convention:
//
//	kernel_crc32c(seed, data) == ^crc32.Update(^seed, castagnoli, data)
//
// This identity also chains correctly: feeding the result of one call as
// the seed of the next is equivalent to a single kernel_crc32c over the
// concatenated input, because the inner inversions cancel. So callers can
// keep computing checksums incrementally (e.g. crc32c(seed, group) then
// crc32c(prev, descriptor)) and obtain the same value the kernel produces
// over the joined byte stream.
//
// Pass the ext4 checksum seed (from s_checksum_seed or derived from the
// UUID) for the initial call, or ^uint32(0) for the superblock checksum
// which is seeded with ~0 independently of s_checksum_seed.
func crc32c(crc uint32, data []byte) uint32 {
	return ^crc32.Update(^crc, castagnoli, data)
}
