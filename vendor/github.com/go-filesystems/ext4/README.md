<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-ext4.png" alt="go-filesystems/ext4" width="720"></p>

# ext4 filesystem

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/ext4.svg)](https://pkg.go.dev/github.com/go-filesystems/ext4)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/ext4/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/ext4/actions/workflows/ci.yml)

Package ext4 test helpers and journal

This package contains an in-memory ext4 implementation and test helpers used
by the unit tests. The recent change shortens the critical section in
`Transaction.Commit()` and adds a regression test exercising concurrent
commits against the sidecar journal.

See the tests under [`test/`](./test) for usage examples.

## Overview

Pure-Go read/write access to ext4 filesystem images.
- No root privileges required.
- No external tools required for normal operations (some cross-check tests use `mke2fs`).
- No CGO.

This package supports extents, 64-bit block numbers, flex_bg, directory
htree indexing, CRC32c metadata checksums, and automatic partition detection.
It also reads the classic ext2/ext3 indirect **block map** (single-, double-
and triple-indirect), **inline data** (small files and directories stored in
the inode/xattr area), and **sparse files** (unallocated holes read back as
zeros) — verified against real `mke2fs`-produced ext2/ext3/ext4 images across
block sizes (see `testdata/`).

## Production-readiness improvements (recent)

- Re-entrant locking moved from a `getGID()` hack to explicit owner-token
  `reentrantMutex` APIs, enabling safe reentrancy without relying on
  runtime internals.
- Metadata serialization for block-group descriptors (BGD), block and inode
  bitmaps and inode-table writes is implemented; `metadata_csum` support is
  available for checksum-protected metadata.
- Online resize: `Grow()` implements expanding images and adding block groups.
- IOCTLs: basic `GetFlags`/`SetFlags` ioctl paths are implemented where useful
  for tooling.
- Filesystem inspection and repair hooks: `CheckImage()` and `RepairImage()`
  provide programmatic fsck/repair entry points for integration tests.
- Concurrency hardening: bitmap/BGD/inode-table updates are serialized with
  per-group locks to avoid concurrent write corruption.
- Tests: unit and integration tests were added for `Grow`, repair paths and
  concurrent bitmap updates; many code paths are covered under the `test`
  build tag.

Important notes

- Production builds compile without the `test` build tag; test-only tracing and
  write hooks are guarded by `//go:build test` and are excluded from production
  binaries.
- The previous `getGID()` test-only helper has been replaced with an owner-token
  based `reentrantMutex` API. Code should prefer `LockOwner/UnlockOwner` with an
  explicit token (see package docs and tests).
- Test wrappers used by `//go:build test` expose an `UnderlyingFile()` helper
  (for example the `loggingRW` wrapper) so that lock keying can share the
  underlying descriptor without using unsafe reflection into unexported fields.

## References

https://docs.kernel.org/filesystems/ext4/

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Supports partitioned images (MBR/GPT auto-detect) |
| Format | ✅ | Creates ext4 images; some cross-check tests require `mke2fs` |
| ReadFile / WriteFile | ✅ | Full file I/O (extents and ext2/3 block map); sparse-file holes read back as zeros |
| Read at an offset | ✅ | `OpenFile(path)` → `io.ReaderAt` + `Size()` (`filesystem.Opener` / `filesystem.File`). Snapshots the inode's block map — extents, ext2/3 indirect map, or inline data — and reads no file data at open, so a 4 KiB read out of a 4 GiB file costs 4 KiB. Holes read as zeros, as `ReadFile` produces them |
| Write at an offset | ✅ | `OpenFile(path)` → `io.WriterAt` + `Truncate` + `Sync` (`filesystem.WritableFile`), for extent-mapped inodes. Writes only the bytes given, allocates only the blocks the range touches, and **keeps the file sparse**: a write far past the end leaves the gap a hole. Inline-data and ext2/3 block-map inodes come back as a plain read-only `File`, so a caller falls back rather than failing |
| Inline data | ✅ | Reads small files and directories stored inline in the inode/xattr area |
| MkDir / Delete / Rename | ✅ | Directory and rename operations implemented |
| ReadLink / Symlinks | ✅ | Fast (in-inode) and slow (out-of-line) targets |
| Metadata serialization (BGD/bitmaps/inode-table) | ✅ | Writes + `metadata_csum` supported |
| Online resize (`Grow` / `Shrink` / `Resize`) | ✅ | `Grow` adds block groups; `Shrink` frees already-free trailing groups (non-relocating, resize2fs-style); `Resize` dispatches to whichever direction the new size requires. `Open`/`Format` return `filesystem.Filesystem`, so callers reach these by type-asserting to an interface exposing the method, e.g. `fs.(interface{ Resize(int64) error })` |
| IOCTLs (GetFlags/SetFlags) | ✅ | Tooling-level ioctl support implemented |
| CheckImage / RepairImage hooks | ✅ | Programmatic fsck/repair entry points |
| Concurrency hardening | ✅ | Per-group locks for bitmap/BGD/inode-table |
| Test-only tracing | ✅ | Heavy tracing under `//go:build test` for diagnostics |

## Reading part of a file

`ReadFile` returns the whole file, which is unusable for anything serving reads on
demand — a mount, an NFS or 9P export — where a 4 KiB request out of a 4 GiB file
must not allocate 4 GiB. The driver implements the optional
[`filesystem.Opener`](https://github.com/go-filesystems/interface) capability:

```go
if o, ok := fs.(filesystem.Opener); ok {
    f, err := o.OpenFile("/var/lib/big.img")
    if err != nil { /* ... */ }
    defer f.Close()

    buf := make([]byte, 4096)
    n, err := f.ReadAt(buf, 1<<30) // only the bytes asked for
    _, _ = n, err
    _ = f.Size()                   // i_size, read at open; touches no data
}
```

`OpenFile` snapshots the inode's block map — extent leaves for ext4, the synthesised
equivalent for the ext2/ext3 indirect map, or the inline bytes for an inline-data
inode — using the same `inode.readExtents` the `ReadFile` path uses, under the same
inode lock. `ReadAt` binary-searches that map, reads whole contiguous runs in one go,
and serves holes as zeros without touching the device. It follows `io.ReaderAt`
exactly (`n < len(p)` only with a non-nil error, `io.EOF` at the end) and is safe to
call concurrently.

A read-only `File` is a **snapshot**: it describes the file as it was when opened. A
file rewritten through the same `Filesystem` afterwards must be reopened.

## Writing part of a file

`WriteFile` replaces a whole file — and this driver's `WriteFile` frees every block
of it and reallocates them — so a positional write expressed as `ReadFile` + splice
+ `WriteFile` costs O(filesize) *per request*, and a client writing a file in
fixed-size blocks pays that per block. Measured over a real Linux kernel NFS mount
against a driver without a positional write, 2 MiB in 64 KiB blocks took 23 s
(90 kB/s), and a `soft,timeo=50` mount gave up with `EIO` partway through.

The `File` returned by `OpenFile` is therefore also a
[`filesystem.WritableFile`](https://github.com/go-filesystems/interface) — when the
inode's layout allows it:

```go
if o, ok := fs.(filesystem.Opener); ok {
    f, _ := o.OpenFile("/var/lib/big.img")
    defer f.Close()
    if w, ok := f.(filesystem.WritableFile); ok {
        _, err := w.WriteAt(buf, 1<<30) // io.WriterAt semantics, exactly
        _ = w.Truncate(1 << 20)         // grow is free; shrink frees blocks
        _ = w.Sync()                    // fsync(2) on a file-backed image
        _ = err
    }
    // otherwise: ReadFile, splice, WriteFile — correct, and quadratic.
}
```

`WriteAt` reuses the extent map resolved at open, turning an offset into a physical
block by binary search, and allocates only for the blocks the written range actually
covers. **Sparseness is preserved**: a write far past the end allocates nothing for
the gap, which stays a hole, costs no space, and reads back as zeros through both
`ReadAt` and `ReadFile`. That is what ext4 means by a sparse file, and it is the
one place this driver's `WriteAt` deliberately differs from FAT's, which has to
allocate the gap.

**Two layouts come back as a plain, read-only `File`**: an inline-data inode, and
the classic ext2/ext3 indirect block map, whose growth would mean allocating
indirect blocks that no write path in this package has ever produced. They are
refused *at the probe* rather than from `WriteAt`, because the type assertion is a
caller's only chance to learn the truth before it commits to a strategy — a `File`
that satisfied the interface and then failed every call would force a server to
discover the limitation one failed request at a time. Refusing at the probe is the
difference between "slower" and "broken".

## Limitations & cautions

- This package is intended primarily for tooling and tests (image creation,
  inspection, repair, and offline modification). It is not a kernel filesystem
  driver and does not aim to be a drop-in replacement for the in-kernel ext4
  implementation.
- Not every kernel ioctl or obscure ext4 feature is implemented; check tests
  and source for the supported surface.
- Integration tests that cross-validate behavior against `e2fsprogs` (`mke2fs`,
  `e2fsck`) are provided but may be skipped on platforms lacking those tools.
- Performance and large-scale concurrency behavior are still subjects for
  benchmarking; see [`BENCHMARKS.md`](./BENCHMARKS.md) and the "Backoff
  tuning summary" section below.

## Module

```bash
github.com/go-filesystems/ext4
```

## Volume label

The driver implements the optional `filesystem.Labeller` interface
so callers can read and update the ext2/3/4 superblock's
`s_volume_name` in place:

```go
fs, _ := ext4.Open("disk.img", -1)
defer fs.Close()
if l, ok := fs.(filesystem.Labeller); ok {
    fmt.Println("label:", l.Label())
    _ = l.SetLabel("cloudimg-rootfs")
}
```

`SetLabel` is **offline-safe only** — it takes the FS write lock,
re-reads the on-disk superblock, zero-fills bytes [120:136], writes
the new label, recomputes the superblock CRC-32C if `metadata_csum`
is enabled, and `WriteAt`+`Sync`s the 1024-byte block back.
Concurrent writers may produce a torn superblock; use it on a
filesystem no other process is actively mutating.

The superblock checksum uses the **kernel-canonical CRC-32C
convention** — `crc32c(~0, sb[:0x3FC])`, no final XOR, no seed —
distinct from every other metadata structure in ext4 (which is
seeded from `s_checksum_seed` or `crc32c(~0, uuid)`). Mismatching
the convention produces "Found ext4 filesystem with invalid
superblock checksum" on the next Linux mount.

## Quick start — validation and best practices

### 1. Static analysis

```bash
go vet ./...
# staticcheck ./...  # run if staticcheck is installed
```

### 2. Run tests with the race detector (test-only tracing requires `-tags test`)

Recommended quick run (from the repo root):

```bash
go test -race -tags test ./... -v
```

Run the test harness package (shorter):

```bash
go test -race -tags test ./test -v
```

For the whole repository (long), saving output to a local log:

```bash
go test -race ./... 2>&1 | tee all_tests_race.log
```

To check coverage for the package:

```bash
go test -tags test -coverprofile=cover.out . && go tool cover -html=cover.out
```

### 3. Build without test tags to ensure no test-only dependencies leak

```bash
go build .
```

## What's changed (high level)

- Replaced `getGID()`-based reentrancy with owner-token `reentrantMutex` APIs.
- Implemented metadata serialization for BGD / bitmaps / inode-table and
  `metadata_csum` support.
- Added `Grow()` to perform online image expansions (adds block groups,
  updates superblock/BGD tables).
- Added basic ioctl support for common tooling flags.
- Exposed `CheckImage()` / `RepairImage()` hooks for programmatic fsck/repair.
- Hardened concurrency around bitmap/BGD/inode-table updates and added tests
  that exercise concurrent bitmap updates.

- Local fixes (developer notes):
  - Cached debug/lock flags to avoid hot-path `os.Getenv` calls.
  - Added a configurable idle timeout for `commitWorker` goroutines to
    prevent unbounded worker proliferation under stress (`EXT4_COMMIT_WORKER_IDLE_SEC`).
  - Fixed commit-sequence stall: `Transaction.Commit()` now guarantees
    `applySeq` always advances (via `mustAdvanceApplySeq` deferred flag)
    even when apply returns an error, preventing all goroutines from
    blocking indefinitely on `dirML.Lock()`.
  - Stress harness adjusted: removed inter-test `t.Parallel()` and
    reduced default workload (workers, ops, write_hammer) to complete
    within the standard `go test` 10-minute timeout.
  - Verified targeted and package-level tests under `-race -tags test` after changes.

## Backoff tuning summary

We performed a small parameter sweep to reduce lock-order contention and
blocking fallbacks observed under the `TestStress_Concurrent` harness. The
table below summarizes the long-run (32 workers × 500 ops) fallback counts
for a few candidate `(attempts, base_us)` values. Results are noisy and
depend on workload timing; treat them as guidance for tuning.

| Attempts | Base (µs) | Total fallbacks (32×500) | Top hot-group | Top count | Notes |
|---:|---:|---:|---|---:|---|
| 4 | 250 | 435 | fd:4:inode:g:3:block:false | 265 | Lower peak but higher total than 6,250 |
| 6 | 250 | 425 | fd:4:inode:g:3:block:false | 294 | Recommended: lowest total fallbacks |
| 8 | 250 | 437 | fd:4:inode:g:3:block:false | 341 | Higher total, higher peak |

Tradeoffs:
- Lower `attempts` can reduce contention bursts (lower peak on hot-group),
  but may increase total blocking fallbacks across the run.
- Higher `attempts` with jitter spreads retries but can increase aggregate
  attempts and CPU; balance to minimize total fallbacks for your workload.

Reproduce a single data point locally (example: attempts=6, base=250µs) from
the repo root:

```bash
EXT4_BACKOFF_MAX_ATTEMPTS=6 EXT4_BACKOFF_BASE_US=250 \
  EXT4_LOCK_DEBUG=1 EXT4_STRESS_WORKERS=32 EXT4_STRESS_OPS_PER_WORKER=500 \
  go test -count=1 -run 'TestStress_Concurrent/debian/concurrent_rw' -tags test ./test -v
```

The sweep above was produced by varying `EXT4_BACKOFF_MAX_ATTEMPTS` /
`EXT4_BACKOFF_BASE_US` across the same command and comparing the printed
fallback counts; no grid-running script ships in this repository, so a full
sweep needs to be scripted by the caller.

Next tuning options:
- Run a denser grid around `(6,250)` to refine the minimum.
- Increase allocation dispersion (`allocGroupCursor`) or subdivide BGD
  locks to reduce serialization on very hot groups.

For questions or to request additional test runs (benchmarks, long-running
integration), open an issue on this repository.
