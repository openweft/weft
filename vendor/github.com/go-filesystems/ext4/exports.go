package filesystem_ext4

// FS is a public alias for the concrete ext4 filesystem type. Used by
// downstream packages (apple-vz/grub) that need to refer to the
// concrete type for method receivers. The Ext4FS alias (same target)
// is gated behind `//go:build test` for test-only helpers; FS is
// always available so production code can hold a typed pointer.
type FS = ext4FS
