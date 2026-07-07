# Performance parity — go-filesystems/ext4 vs mkfs.ext4 / kernel ext4 / debugfs  (2026-06-22)

## Methodology

- **Where**: the `debian` Tart VM (linux/arm64) running on an Apple-silicon (M4)
  host. Our pure-Go driver **and** the reference C tools run inside the same VM,
  on the same hardware and the same Linux kernel, so cold-cache reads are
  directly comparable (`echo 3 > /proc/sys/vm/drop_caches` before every read
  iteration).
- **CPU / kernel**: 4 vCPU aarch64, Linux 6.12.74 (Debian 13).
- **Go**: 1.26.4 linux/arm64. CGO disabled (pure Go).
- **Reference tools**: e2fsprogs 1.47.2 (`mkfs.ext4`, `debugfs`), in-tree kernel
  ext4.
- **Image set**: 2008 files — 2000 small (1–4 KiB, 50 dirs) + 8 large (4 MiB)
  ≈ 38 MB of file data in a 111 MiB image.
- **Sampling**: best-of-5. Format and read are timed separately. Read iterations
  are **cold** (caches dropped). Throughput uses the file-data payload
  (~38 MB), not the image size.
- **Format**: ours `ext4.Format(path, size, cfg)` vs `truncate` + `mkfs.ext4`.
- **Read**: the image is created and populated by `mkfs.ext4` + loop-mount +
  `cp -a` (the kernel's own writer), then read three ways — ours
  (`Open` + recursive `ListDir`/`Stat`/`ReadFile`), the kernel (`mount -o loop`
  + `tar -cf /dev/null`), and the userspace peer `debugfs -R "rdump / …"`.
- **Correctness gate (verified)**: our extraction returns exactly 2008 files and
  byte-for-byte the same payload as the source; **our `ext4.Format` output
  loop-mounts cleanly and is `fsck.ext4 -fn` clean.**

## Results

| op | size | ours (MB/s, wall) | reference (MB/s, wall) | ratio | verdict |
|----|------|-------------------|------------------------|-------|---------|
| Format | 111 MiB | — , **8.98 ms** | mkfs.ext4: — , 38.85 ms | **0.23×** | **ours 4.3× faster** |
| Read (cold) | 38 MB | 209 MB/s, 175.7 ms | kernel: 867 MB/s, 42.4 ms | 4.15× | ours 4.15× slower |
| Read (cold) | 38 MB | 209 MB/s, 175.7 ms | debugfs: 494 MB/s, 74.4 ms | 2.36× | ours 2.36× slower |

## Summary

- **Format: we win (4.3×).** Our `Format` writes a sparse, metadata-only image;
  `mkfs.ext4` does more up-front work (lazy-init tables, journal, multiple
  backup superblocks). Both produce an fsck-clean, mountable filesystem, so for
  image *provisioning* our approach is genuinely faster on this geometry.
- **Read: we lag the kernel 4.15× and the userspace peer (`debugfs`) 2.36×.**
  This is the honest gap and the place to invest.

### Root causes (read)

1. **No readahead / batched I/O.** Every block goes through a separate
   `io.ReaderAt.ReadAt`. The kernel issues large readahead windows; we issue one
   syscall per extent block.
2. **Per-call allocation.** `ReadFile` allocates a fresh buffer per file and the
   extent walk allocates per block. With 2008 files this is heavy GC pressure.
3. **Scalar CRC.** Metadata checksum verification uses
   `hash/crc32` (Castagnoli) without the SIMD path the kernel gets for free.
4. **Directory + inode lookups are not cached** across the walk.

### Action items

- [ ] Batch adjacent extent blocks into a single `ReadAt` (coalesce contiguous
      runs) — biggest expected win for the 4 MiB files.
- [ ] Add a small block/inode cache so the directory walk does not re-read group
      descriptors and bitmaps.
- [ ] Pool read buffers (`sync.Pool`) to cut allocations and GC.
- [ ] Accelerate crc32c via go-asmgen SIMD on the 6 target arches (shared with
      the other go-filesystems drivers).
- [ ] Optional parallel extract (worker pool over files) for multi-core hosts.

## Reproduce

```sh
# inside the debian VM, with e2fsprogs installed and Go on PATH
sudo ./benchmarks/run.sh ext4 <repo_dir> <work_dir> 5
```

`benchmarks/run.sh` is filesystem-agnostic (shared across the go-filesystems
drivers); `benchmarks/bench.go` is the ext4-specific timing harness. Both are a
standalone `main` package, excluded from the library coverage gate.
