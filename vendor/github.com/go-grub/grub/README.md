# grub

Pure-Go library for generating and patching GRUB configuration files inside raw disk images. No external tools or root privileges required.

## Module

```
github.com/openweft/grub
```

Depends on [`ext4`](../go-filesystems/ext4). Internal call sites
type-assert the `filesystem.Filesystem` returned by `ext4.Open()` to
`*ext4.FS` to reach ext4-specific helpers — this requires the
`FS = ext4FS` alias the ext4 driver exposes in its non-test
`exports.go`.

## Overview

`MkConfig` mirrors what `grub-mkconfig` does on a live system: it reads `/etc/default/grub` and all `/etc/default/grub.d/*.cfg` drop-ins **from inside the disk image**, merges the resulting environment variables, then applies `GRUB_CMDLINE_LINUX_DEFAULT` to `/boot/grub/grub.cfg` (or `/boot/grub2/grub.cfg` on Red Hat derivatives). Nothing is hardcoded — the effective configuration is always derived from what is present in the image at the time of the call.

`ApplyFileOps` writes one or more files into the image and optionally triggers `MkConfig` after all writes are complete.

## API

```go
// FileOp describes a single file to write into a disk image with an optional
// post-write trigger. The only recognised trigger is "grub-mkconfig".
type FileOp struct {
    Content string
    Dst     string
    Trigger string // "" | "grub-mkconfig"
}

// ApplyFileOps writes each FileOp into the ext4 image at imagePath, then runs
// any requested triggers (deduped).
func ApplyFileOps(imagePath string, ops []FileOp) error

// MkConfig regenerates grub.cfg inside the image by sourcing the GRUB defaults
// from /etc/default/grub and /etc/default/grub.d/*.cfg drop-ins.
func MkConfig(imagePath string) error

// PatchCfgContent applies extraArgs to all linux / linux16 lines in a grub.cfg
// file content and returns the patched result.
func PatchCfgContent(content string, extraArgs []string) string
```

## Usage

### Regenerate grub.cfg after writing a drop-in

```go
import "github.com/openweft/grub"

err := grub.ApplyFileOps("/path/to/disk.img", []grub.FileOp{
    {
        Content: "GRUB_CMDLINE_LINUX_DEFAULT=\"console=tty0 console=ttyS0,115200\"\n",
        Dst:     "/etc/default/grub.d/99-console.cfg",
        Trigger: "grub-mkconfig",
    },
})
```

### Regenerate grub.cfg directly

```go
err := grub.MkConfig("/path/to/disk.img")
```

## Used by

- [`pkg/openweft/weft`](../apple-vz/vzd) — patches GRUB config during VM provisioning
- [`pkg/grubc`](../grubc) — CLI wrapper
