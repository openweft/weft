//go:build !test
// +build !test

package filesystem_ext4

// In non-test builds, just return the raw *os.File for performance.
func getRW(fs *ext4FS) readerWriterAt { return fs.f }
