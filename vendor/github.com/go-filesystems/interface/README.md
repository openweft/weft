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

Known implementations in this repository:

- `github.com/go-filesystems/xfs`
- `github.com/go-filesystems/zfs`
- `github.com/go-filesystems/ntfs`
- `github.com/go-filesystems/ext4`
- `github.com/go-filesystems/btrfs`
- `github.com/go-filesystems/fat32`
- `github.com/go-filesystems/exfat`

See each implementor's README for format-specific details and examples. For
example: [xfs](../xfs/README.md), [zfs](../zfs/README.md).

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
