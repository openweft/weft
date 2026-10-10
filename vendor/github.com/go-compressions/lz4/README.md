<p align="center"><img src="https://raw.githubusercontent.com/go-compressions/brand/main/social/go-compressions-lz4.png" alt="go-compressions/lz4" width="720"></p>

# lz4

[![ci](https://github.com/go-compressions/lz4/actions/workflows/ci.yml/badge.svg)](https://github.com/go-compressions/lz4/actions/workflows/ci.yml)
![coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-compressions/lz4.svg)](https://pkg.go.dev/github.com/go-compressions/lz4)

A clean Go implementation of the **LZ4 block format** (`CompressBlock` /
`DecompressBlock`), wire-compatible with the reference (cross-checked against
`pierrec/lz4`). Its compressor delegates LZ4's hot "count the matching bytes"
inner loop (`LZ4_count`) to [matchlen](https://github.com/go-simd/matchlen),
whose SIMD common-prefix kernel makes match extension fast.

As of `matchlen` **v0.3.0**, that kernel ships SIMD on **all six** of Go's 64-bit
targets — amd64 (SSE2), arm64 (NEON), riscv64 (RVV), loong64 (LSX), ppc64le (VSX)
and s390x (vector facility). lz4 needs **no code change** to benefit: `MatchLen`
dispatches per-arch, so the bulk match now runs vectorized on ppc64le and s390x
too. **ppc64le is now natively measured on real POWER9** (GCC Compile Farm, VSX,
Go 1.26.4, 2026-06-26): encode ~1.8× scalar (1174 vs 644 MB/s) and it **beats
`pierrec/lz4`** there (1174 vs 1012 MB/s) — the matchlen-accelerated extension
pays off. **riscv64 is now natively measured too** on a SpacemiT X60 (RVV 1.0, a
low-power in-order core — the only widely-available RVV silicon; GCC Compile
Farm, Go 1.26.4, 2026-06-26): encode **~1.45× scalar (110 vs 76 MB/s)** and it
**beats `pierrec/lz4`** there (110 vs 83 MB/s → ~1.32×); an out-of-order RVV core
would likely do better. s390x stays qemu-validated for correctness only; native s390x
throughput is pending (no GitHub-hosted IBM Z runner). The library is also
build+test validated bit-exact on **ppc64 (big-endian)** on real POWER9 silicon
(the generic/scalar fallback path) — **six SIMD targets, validated on seven
architectures**.

```go
c := lz4.CompressBlock(src)
out, _ := lz4.DecompressBlock(c, len(src))
```

## Apple LZ4 frame format

macOS's Compression framework (`COMPRESSION_LZ4`) and Apple tooling built on it
— such as [Tart](https://github.com/cirruslabs/tart), whose disk layers are
`.compressed(using: .lz4)` — do **not** emit a bare LZ4 block. They emit a
*frame*: a sequence of `bv41` (compressed), `bv4-` (stored) and `bv4$` (end)
blocks, little-endian, where a compressed block's matches may reference the
previous block's output through a shared **64 KiB sliding window** (LZ4 offsets
are 16-bit). This is the LZ4 sibling of the `bvx*` frame that
[`go-compressions/lzfse`](https://github.com/go-compressions/lzfse) decodes.

```go
// Streaming (bounded memory: one block + the 64 KiB window) — use this for
// large inputs such as Tart disk layers.
n, err := lz4.DecompressAppleStream(dst /* io.Writer */, src /* io.Reader */)

// Whole-buffer convenience.
out, err := lz4.DecompressApple(frameBytes)
```

Validated against real Apple output: a `cirruslabs/macos-sequoia-base` Tart disk
layer decodes byte-for-byte to the length and SHA-256 recorded in its
`org.cirruslabs.tart.uncompressed-*` OCI annotations.

## Why LZ4 is the ideal matchlen consumer

LZ4 has **no entropy-coding stage** — encode time is dominated by match-finding
and extension — so a faster `MatchLen` shows up end-to-end (unlike codecs whose
time goes to FSE/Huffman).

## The compressor

The parse is a single-cell hash table keyed on a **6-byte sequence** (the
reference LZ4 / `pierrec` fast-mode hash, far better dispersed than a 4-byte
key). Each step probes three adjacent positions (`ip`, `ip+1`, `ip+2`) from one
8-byte load, inserting every position so later matches see more candidates, and
ramps its skip distance on incompressible spans. On a hit it applies **lazy
matching** — it peeks one byte ahead and, if `ip+1` yields a strictly longer
match, defers the current one — capped to short matches so the lookahead cost is
only paid where it can help. Match-length extension is delegated to
[matchlen](https://github.com/go-simd/matchlen)'s SIMD kernel.

## Performance

Encoded as single LZ4 blocks, three representative corpora — two text
(Project Gutenberg `pg1661`, Mark Twain) and one binary (a kernel `bzImage`) —
`-count=8` medians, **as of 2026-06-14**.

**Native arm64 (Apple Silicon, this host):**

| corpus | this package | `pierrec/lz4` | speed vs `pierrec` | our size | `pierrec` size | our size vs `pierrec` |
|---|---:|---:|---:|---:|---:|---:|
| text `pg1661` | 208 MB/s | 301 MB/s | 0.69× | **0.528** | 0.553 | **−4.6%** |
| text Twain | 205 MB/s | 285 MB/s | 0.72× | **0.548** | 0.575 | **−4.6%** |
| binary `bzImage` | 249 MB/s | 370 MB/s | 0.67× | **0.638** | 0.653 | **−2.2%** |

**amd64 (QEMU x86_64 lima VM — TCG, so absolutes are low and noisy; the
compressed output is *byte-identical* to arm64 and decodes both ways with
`pierrec`):** our encoder lands at ≈0.5–0.7× `pierrec`'s TCG throughput, with the
same size advantage as above (the parse is deterministic, so sizes match arm64
exactly).

**Honest verdict.** This pass **beat `pierrec` on compression ratio** on every
corpus (text ≈4.6% smaller, binary ≈2.2% smaller) — the 6-byte hash, 3-position
probe and lazy matching are real wins, and they fixed the prior parse, which was
actually **7–8% *worse* than `pierrec` on text**. We did **not** beat `pierrec`
on **speed**: it stays ahead at ≈1.4× (we are at ~0.67–0.72× native arm64). That
gap is the parse/table, not the kernel — `pierrec` uses a half-the-size 16-bit
position table (better cache footprint) and skips lazy matching in fast mode,
trading a little ratio for speed; we make the opposite trade. The SIMD
`matchlen` extension is correct and in use, but match extension is only ~10% of
encode time here — the bottleneck is match-*finding*. Blocks remain mutually
decodable with `pierrec` in both directions, verified on arm64 and amd64.
(`matchlen` ships SIMD on all six 64-bit Go targets. On **native POWER9**
ppc64le this encoder reaches **1174 MB/s — ~1.8× scalar and ahead of `pierrec`'s
1012 MB/s** there, since match-finding is comparatively less dominant on that
core; on **native riscv64** (SpacemiT X60, RVV 1.0 — a low-power in-order core,
the only widely-available RVV silicon) it reaches **110 MB/s — ~1.45× scalar and
ahead of `pierrec`'s 83 MB/s** (~1.32×); s390x native throughput stays pending.
Build+test validated bit-exact on ppc64 big-endian too — six SIMD targets, seven
validated architectures.)

## License

BSD-3-Clause.
