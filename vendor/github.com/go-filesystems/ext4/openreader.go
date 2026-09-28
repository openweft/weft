// SPDX-License-Identifier: BSD-3-Clause

package filesystem_ext4

import (
	"errors"
	"fmt"
	"io"

	filesystem "github.com/go-filesystems/interface"
)

// OpenReader opens the ext4 filesystem that starts at offset zero of r.
//
// It exists so this driver fits the one shape the rest of go-filesystems
// agrees on: github.com/go-filesystems/detect's Opener is
// func(io.ReaderAt, int64) (filesystem.Filesystem, error). OpenFromDevice was
// already close, but its parameter is an UNEXPORTED interface -- no caller
// outside this package can name a type that satisfies it, so in practice the
// only way in was a path.
//
// There is no partition index: r is expected to begin at the filesystem, which
// is what detect has just established by reading the magic there. Open remains
// the way to look through a partition table.
//
// WRITES need an io.WriterAt. A reader that is only a reader gives a
// filesystem that serves every read and refuses every write with
// ErrReadOnlyReader -- rather than one that accepts a write and loses it.
//
// The reader is NOT closed by Close: the caller opened it and still owns it.
// That is the same division as iso9660, squashfs, hfsplus and ufs, whose
// reader-based constructors have always worked this way.
func OpenReader(r io.ReaderAt, size int64) (filesystem.Filesystem, error) {
	if r == nil {
		return nil, errors.New("ext4: OpenReader needs a reader")
	}
	if size <= 0 {
		return nil, fmt.Errorf("ext4: an image of %d bytes is not one", size)
	}
	return openFromDevice(&readerDevice{r: r, size: size}, -1)
}

// ErrReadOnlyReader is what a write gets when the filesystem was opened over
// something that can only be read.
var ErrReadOnlyReader = errors.New("ext4: this filesystem was opened over a reader that cannot be written")

// readerDevice presents whatever the caller passed as the block device this
// driver reads through. The write side is present only when the reader has
// one, and refuses by name when it does not.
type readerDevice struct {
	r    io.ReaderAt
	size int64
}

func (d *readerDevice) ReadAt(p []byte, off int64) (int, error) { return d.r.ReadAt(p, off) }

func (d *readerDevice) WriteAt(p []byte, off int64) (int, error) {
	w, ok := d.r.(io.WriterAt)
	if !ok {
		return 0, ErrReadOnlyReader
	}
	return w.WriteAt(p, off)
}

// Sync reaches the reader's own Sync when it has one -- an *os.File does --
// and is otherwise nothing to do rather than an error: a caller that cannot
// write has nothing to flush.
func (d *readerDevice) Sync() error {
	if s, ok := d.r.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
}

func (d *readerDevice) Size() (int64, error) { return d.size, nil }

// Truncate is refused rather than emulated: the size of the caller's reader is
// the caller's business, and a driver that silently did nothing here would
// report a resize that did not happen.
func (d *readerDevice) Truncate(int64) error {
	return fmt.Errorf("ext4: %w, so it cannot be resized", ErrReadOnlyReader)
}

// Close does not close the reader: see OpenReader.
func (d *readerDevice) Close() error { return nil }
