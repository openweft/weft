<p align="center"><img src="https://raw.githubusercontent.com/go-diskimages/brand/main/social/go-diskimages-qcow2.png" alt="go-diskimages/qcow2" width="720"></p>

# qcow2

[![Go Reference](https://pkg.go.dev/badge/github.com/go-diskimages/qcow2.svg)](https://pkg.go.dev/github.com/go-diskimages/qcow2)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)
[![CI](https://github.com/go-diskimages/qcow2/actions/workflows/ci.yml/badge.svg)](https://github.com/go-diskimages/qcow2/actions/workflows/ci.yml)

Pure-Go reader, creator, and converter for the QEMU **QCOW2** disk image format
(versions 2 and 3). No `qemu-img`, no CGO, no external tools. Works on all
platforms and all supported Go architectures.

## Module

```
github.com/go-diskimages/qcow2
```

The package identifier is `image_qcow2`.

## QCOW2 format

QCOW2 ("QEMU Copy-On-Write, version 2") stores a virtual disk sparsely: only
clusters that have been written are allocated in the file, and clusters may be
optionally compressed. This package reads the big-endian on-disk structures
directly:

- **Header** — magic `QFI\xfb`, version, `cluster_bits`, virtual size,
  encryption method, and the L1 table location.
- **Two-level mapping** — an L1 table points to L2 tables; each L2 entry maps a
  virtual cluster to a file offset (or marks it unallocated → reads as zeros).
- **Refcount table / blocks** — track cluster allocation for growth.
- **Compressed clusters** — deflate (compression type 0). Reading is supported;
  ZSTD (type 1) returns an explicit error.

Encrypted images (`encryption_method != 0`) return an error. Backing files are
not followed: unallocated clusters read back as zeros.

## Public API

### Package-level functions

```go
// Create writes a new empty QCOW2 v2 image with the given virtual size.
// The file allocates no data clusters; reads return zeros.
func Create(path string, sizeBytes int64) error

// IsQCOW2File reports whether path begins with the QCOW2 magic (QFI\xfb).
func IsQCOW2File(path string) bool

// ConvertToRaw reads a QCOW2 v2/v3 image at src and writes an equivalent raw
// disk image to dst, emitting "N%\n" progress lines to w. Deflate-compressed
// clusters are decompressed. Encryption and ZSTD compression return an error.
func ConvertToRaw(src, dst string, w io.Writer) error
```

### Format value

`Format` satisfies the `github.com/go-diskimages/interface` format interface by
structural typing (no import of that module required):

```go
type Format struct{}

func (Format) Name() string                                 // "qcow2"
func (Format) Create(path string, sizeBytes int64) error    // new blank QCOW2 v2 image
func (Format) Detect(path string) (bool, error)             // QCOW2 magic detection
func (Format) ToRaw(src, dst string, w io.Writer) error     // decode QCOW2 → raw
func (Format) Resize(path string, newSizeBytes int64) error // grow only (shrink errors)
```

### Read-write block device

`Device` exposes a QCOW2 image as a random-access block device. Unallocated
clusters are allocated lazily on first write (copy-on-write append). Compressed
clusters are readable; writing to a compressed cluster returns an error
(re-compression is not implemented).

```go
func OpenDevice(path string) (*Device, error)

func (d *Device) ReadAt(p []byte, off int64) (int, error)
func (d *Device) WriteAt(p []byte, off int64) (int, error)
func (d *Device) Size() (int64, error)
func (d *Device) Truncate(size int64) error
func (d *Device) Sync() error
func (d *Device) Fd() uintptr
func (d *Device) Close() error
```

## Examples

### Create, inspect, convert

```go
import image_qcow2 "github.com/go-diskimages/qcow2"

// Create a 4 GiB sparse QCOW2 image.
err := image_qcow2.Create("disk.qcow2", 4<<30)

// Detect.
ok := image_qcow2.IsQCOW2File("disk.qcow2") // true

// Convert to a raw image (progress printed to stdout).
err = image_qcow2.ConvertToRaw("disk.qcow2", "disk.raw", os.Stdout)
```

### Random-access reads and writes

```go
import image_qcow2 "github.com/go-diskimages/qcow2"

dev, err := image_qcow2.OpenDevice("disk.qcow2")
if err != nil {
    return err
}
defer dev.Close()

// Write allocates clusters lazily.
if _, err := dev.WriteAt([]byte("hello"), 1<<20); err != nil {
    return err
}
if err := dev.Sync(); err != nil {
    return err
}

buf := make([]byte, 5)
if _, err := dev.ReadAt(buf, 1<<20); err != nil {
    return err
}
```

## Scope: no raw→QCOW2 encoder

This package deliberately does **not** implement raw→QCOW2 *encoding*
(populated-cluster allocation with compression). The `Format` interface defines
only `ToRaw` (decode QCOW2 → raw), and that decode path — together with
`Create` (blank images) and the `Device` block interface — is the intended
boundary. Producing a compressed QCOW2 image from an arbitrary raw source is
out of scope; use the block `Device` to populate an image cluster-by-cluster, or
convert to raw and hand the raw image to the caller's chosen tooling.

## License

BSD-3-Clause. Copyright the go-diskimages/qcow2 authors.
