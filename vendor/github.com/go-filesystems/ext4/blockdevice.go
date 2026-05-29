package filesystem_ext4

import "os"

// blockDevice is the interface that backs an ext4 filesystem.
// It extends readerWriterAt with lifecycle and capacity operations.
// *os.File satisfies blockDevice via osFileDevice; disk image packages
// (e.g. qcow2) can provide their own implementations.
type blockDevice interface {
	readerWriterAt
	// Sync flushes any buffered writes to the underlying storage.
	Sync() error
	// Size returns the total capacity of the device in bytes.
	Size() (int64, error)
	// Truncate adjusts the capacity of the device to size bytes.
	Truncate(size int64) error
	// Close releases the device.
	Close() error
}

// BlockDevice is the exported alias of blockDevice, lets external
// packages satisfy the interface and pass instances to
// OpenFromDevice. Without it, OpenFromDevice's signature
// `(dev blockDevice, …)` references an unexported type — only
// code inside this package could call it. Use cases for
// implementing BlockDevice externally:
//
//   - LUKS / dm-crypt: wrap a github.com/go-fde/luks.Device so
//     ext4.OpenFromDevice reads the plaintext payload.
//   - In-memory testing: feed a *bytes.Reader-backed fixture
//     into the FS without writing a tempfile.
//   - qcow2 / DMG / other image formats from the go-diskimages
//     family: surface the unpacked block view as a BlockDevice.
type BlockDevice = blockDevice

// osFileDevice wraps an *os.File to satisfy blockDevice.
type osFileDevice struct{ f *os.File }

func (d *osFileDevice) ReadAt(p []byte, off int64) (int, error)  { return d.f.ReadAt(p, off) }
func (d *osFileDevice) WriteAt(p []byte, off int64) (int, error) { return d.f.WriteAt(p, off) }
func (d *osFileDevice) Sync() error                              { return d.f.Sync() }
func (d *osFileDevice) Truncate(size int64) error                { return d.f.Truncate(size) }
func (d *osFileDevice) Close() error                             { return d.f.Close() }

func (d *osFileDevice) Size() (int64, error) {
	fi, err := d.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// UnderlyingFile returns the wrapped *os.File, used by locking helpers and
// journal components that need the raw file descriptor.
func (d *osFileDevice) UnderlyingFile() *os.File { return d.f }

// Name returns the file path, used by test helpers to read image snapshots.
func (d *osFileDevice) Name() string { return d.f.Name() }
