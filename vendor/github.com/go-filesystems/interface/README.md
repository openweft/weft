<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems.png" alt="go-filesystems/interface" width="720"></p>

# filesystem (interface)

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/interface.svg)](https://pkg.go.dev/github.com/go-filesystems/interface)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/interface/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/interface/actions/workflows/ci.yml)

Shared, minimal filesystem interfaces used by the concrete filesystem
implementations in this repository.

## Module

```
github.com/go-filesystems/interface
```

## Purpose

This package defines a small, stable contract that filesystem drivers
implement so higher-level tools can operate on different filesystem images
without depending on concrete types. The interface intentionally focuses on
common file and directory operations needed by tooling and tests.

## API (summary)

- `Filesystem` — minimal filesystem API implemented by concrete packages:

```go
type Filesystem interface {
	Close() error
	ReadFile(path string) ([]byte, error)
	ListDir(path string) ([]DirEntry, error)
	Stat(path string) (Stat, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	ReadLink(path string) (string, error)
	MkDir(path string, perm os.FileMode) error
	DeleteFile(path string) error
	DeleteDir(path string) error
	Rename(oldPath, newPath string) error
}
```

- `Labeller` — optional interface for filesystems that expose a
  volume label. Implementations cap the label at their own length
  limit (ext2/3/4: 16 bytes; FAT: 11 bytes). Probe via type
  assertion; not part of `Filesystem` because some filesystems
  genuinely have no label concept or where label mutation is
  non-trivial.

```go
type Labeller interface {
    Label() string
    SetLabel(label string) error
}

if l, ok := fs.(filesystem.Labeller); ok {
    l.SetLabel("rootfs")
}
```

- `LabelReader` — optional read-only counterpart to `Labeller`, for drivers
  that can decode a volume label but can't yet rewrite it through their
  regular commit machinery. Every `Labeller` embeds this, so type-assert
  `LabelReader` when only reading is needed:

```go
type LabelReader interface {
    Label() string
}
```

- `Symlinker` — optional interface for creating symbolic links (`ReadLink`
  is already part of `Filesystem`; this gates the write side):

```go
type Symlinker interface {
    Symlink(target, linkPath string) error
}
```

- `HardLinker` — optional interface for POSIX hardlinks (directories cannot
  be hardlinked; implementations must reject that case):

```go
type HardLinker interface {
    Link(oldPath, newPath string) error
}
```

- `MetadataSetter` — optional interface bundling the POSIX metadata mutators
  (chmod / chown / utimes):

```go
type MetadataSetter interface {
    Chmod(path string, perm os.FileMode) error
    Chown(path string, uid, gid uint32) error
    Chtimes(path string, atime, mtime time.Time) error
}
```

- `Truncater` — optional interface for resizing a regular file in place
  (grow zero-fills, shrink drops trailing data):

```go
type Truncater interface {
    Truncate(path string, newSize int64) error
}
```

- `Grower` / `Resizer` — optional interfaces for changing a filesystem's
  on-disk size. `Grower.GrowTo` is grow-only; `Resizer.Resize` is the newer,
  uniform entry point that also handles shrink where the format allows it,
  returning the sentinel `ErrShrinkUnsupported` when it doesn't:

```go
type Grower interface {
    GrowTo(newSizeBytes int64) error
}

type Resizer interface {
    Resize(newSize int64) error
}

var ErrShrinkUnsupported = errors.New("filesystem: shrink not supported")

if r, ok := fs.(filesystem.Resizer); ok {
    if err := r.Resize(newSize); errors.Is(err, filesystem.ErrShrinkUnsupported) {
        // driver only grows — caller decides how to handle
    }
}
```

- `Opener` / `File` — optional interface for **reading part of a file** without
  materialising the whole thing. `Filesystem.ReadFile` is per-path and returns
  the entire file, which no mount or network export can build on: serving a
  4 KiB request out of a 4 GiB file would allocate 4 GiB. `Opener` is the
  capability that makes those callers possible, and it is optional and
  non-breaking — probe for it, fall back to `ReadFile` when a driver lacks it:

```go
type Opener interface {
    OpenFile(path string) (File, error)
}

type File interface {
    io.ReaderAt
    io.Closer
    Size() int64
}

if o, ok := fs.(filesystem.Opener); ok {
    f, err := o.OpenFile("/big.img")
    if err != nil {
        return err
    }
    defer f.Close()
    n, err := f.ReadAt(buf, off) // only the bytes asked for
    _, _ = n, err
} else {
    data, err := fs.ReadFile("/big.img") // fallback: the whole file
    _, _ = data, err
}
```

  `ReadAt` follows `io.ReaderAt` **to the letter**: `n < len(p)` only ever comes
  back with a non-nil error, end of file reports `io.EOF`, and an offset at or
  past `Size()` returns `0, io.EOF`. Anything looser breaks `io.SectionReader`
  and every consumer built on it, silently. Reads are safe to issue
  concurrently on one `File`, as `io.ReaderAt` requires. `Size()` comes from
  metadata already read at open time, so a caller can answer a stat without
  touching the data.

  A driver whose format genuinely cannot answer a byte range without decoding
  everything before it should simply *not* implement `Opener`, rather than
  emulate it with a hidden full read — the probe exists so the caller learns
  the truth.

- `WritableFile` — the **write-side twin** of `Opener`/`File`: the optional
  upgrade of a `File`, for a driver that can **write at an offset in place**.
  `Filesystem`'s only write is `WriteFile`, which replaces the *whole* file, so
  a caller handed a write at a non-zero offset must read the file, splice, and
  write it all back — O(filesize) per request, and O(n²) for a client streaming
  a file in blocks. Measured, not supposed: a 2 MiB sequential write over a real
  NFS mount in 64 KiB blocks took **23 s (90 kB/s)**, and a `soft` mount gave up
  with `EIO` partway through.

```go
type WritableFile interface {
    File            // io.ReaderAt + io.Closer + Size() int64
    io.WriterAt
    Truncate(size int64) error
    Sync() error
}

f, err := o.OpenFile(path)
if err != nil {
    return err
}
defer f.Close()
if w, ok := f.(filesystem.WritableFile); ok {
    if _, err := w.WriteAt(p, off); err != nil { // only the bytes given
        return err
    }
    return w.Sync()
}
// otherwise: ReadFile, splice, WriteFile — correct, and quadratic.
```

  The probe is on the **`File`**, not on the `Filesystem`, because writability
  is a property of the opened object. A read-only driver (`iso9660`,
  `squashfs`) simply returns a plain `File` and nothing breaks: `Filesystem`,
  `Opener` and `File` are all unchanged.

  `WriteAt` follows `io.WriterAt` **to the letter**: it writes `len(p)` bytes
  or returns a non-nil error — never a short write with a nil error, which a
  caller reads as success. Whether it may *extend* the file is the driver's to
  state, and each one states it; a driver that cannot extend refuses the write
  rather than writing short. Concurrent `WriteAt` calls are safe **provided
  their ranges do not overlap**, which is `io.WriterAt`'s own wording.

  `Size()` on a `WritableFile` is *not* the frozen snapshot `File` describes:
  it reflects this handle's own writes, so it follows an extending `WriteAt` or
  a `Truncate` with no I/O and no reopen.

  `Sync()` exists because a network server must be able to answer *"it is on
  the medium"* as an event distinct from *"I accepted the write"* — NFSv3
  `COMMIT` (RFC 1813 §3.21) and SFTP's `fsync@openssh.com` both demand exactly
  that answer. A driver whose `Sync` guarantees nothing must say so, rather
  than return `nil` and let the caller promise durability it has not got.

- `DirEntry` — accessor interface for directory entries:

```go
type DirEntry interface {
	Inode() uint64
	Name() string
	FileType() uint8
}
```

- `Stat` — file metadata accessor:

```go
type Stat interface {
	Mode() uint16
	Size() uint64
	Inode() uint64
}
```

Constructors `NewDirEntry(inode, name, fileType)` and `NewStat(mode,size,inode)` are
provided for convenience.

## Implementations

Known implementations, across the [`go-filesystems`](https://github.com/go-filesystems) org:

- `github.com/go-filesystems/apfs`
- `github.com/go-filesystems/btrfs`
- `github.com/go-filesystems/exfat`
- `github.com/go-filesystems/ext4`
- `github.com/go-filesystems/fat32`
- `github.com/go-filesystems/ffs` (re-export of `ufs`)
- `github.com/go-filesystems/hfsplus`
- `github.com/go-filesystems/iso9660`
- `github.com/go-filesystems/ntfs`
- `github.com/go-filesystems/oci`
- `github.com/go-filesystems/squashfs`
- `github.com/go-filesystems/uefi`
- `github.com/go-filesystems/ufs`
- `github.com/go-filesystems/xfs`
- `github.com/go-filesystems/zfs`

See each implementor's README for format-specific details, examples, and
which optional interfaces above it satisfies; or see
[go-filesystems.github.io/docs/drivers](https://go-filesystems.github.io/docs/drivers/)
for a capability matrix. `github.com/go-filesystems/detect` composes over
this interface too, as a type-probing dispatch registry rather than a driver.

## Usage example

The interface can be used as a programming abstraction so callers accept a
`filesystem.Filesystem` regardless of the concrete implementation:

```go
import (
	filesystem "github.com/go-filesystems/interface"
	fsx "github.com/go-filesystems/xfs"
)

func example() error {
	f, err := fsx.Open("image.img", -1)
	if err != nil {
		return err
	}
	defer f.Close()

	// Use as the generic interface
	var fs filesystem.Filesystem = f
	data, err := fs.ReadFile("/hello.txt")
	if err != nil {
		return err
	}
	_ = data
	return nil
}
```

## Notes

Keep the interface minimal; add helpers in implementor packages when
format-specific functionality is required.
