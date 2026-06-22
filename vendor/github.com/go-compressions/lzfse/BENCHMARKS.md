# Performance parity — go-compressions/lzfse vs Apple reference lzfse  (2026-06-22)

**Methodology**

- **Host:** Apple M4 Max, macOS 26.5, single core (no parallelism in either codec).
- **Our codec:** `github.com/go-compressions/lzfse` built with Go 1.26.4, `CGO_ENABLED=0`.
- **Reference:** Apple's `github.com/lzfse/lzfse` C library (commit `e634ca5`), built
  `-O3` (Apple ships `-Os`; `-O3` gives the reference its best speed — the fairest
  "authoritative bar"). Timed **in-memory** via `benchmarks/ref/lzfse_bench.c` linked
  against `liblzfse`, so both sides time pure in-process encode/decode of a resident
  buffer — no file or process I/O on either side.
- **Corpus:** the Silesia corpus (text / binary / database / structured) plus three
  synthetic 8 MiB edge cases (zeros, random, highly repetitive).
- **Iterations:** 21 timed iterations per file after 3 warm-up rounds; the table reports
  **best** (lowest-noise) throughput. Ratio = compressed ÷ original (lower is better).
- **Correctness:** every file is round-trip verified (`Decompress(Compress(x)) == x`)
  before timing — the `rt` column. (Byte-level cross-compatibility with Apple's stream
  is a separate matter; see "Cross-compatibility" below.)

| file | our comp MB/s | ref comp MB/s | our decomp MB/s | ref decomp MB/s | our ratio | ref ratio | rt |
|------|--------------:|--------------:|----------------:|----------------:|----------:|----------:|:--:|
| dickens             |  29.0 |  114.6 |   158.1 |  1234.0 | 0.391 | 0.379 | ok |
| mozilla             |  36.6 |  177.1 |   122.5 |  1342.0 | 0.370 | 0.367 | ok |
| mr                  |  45.2 |  170.2 |   223.1 |  1240.3 | 0.363 | 0.359 | ok |
| nci                 |  96.0 |  286.6 |   384.1 |  3903.8 | 0.099 | 0.096 | ok |
| ooffice             |  39.9 |  139.1 |   224.2 |  1142.5 | 0.508 | 0.505 | ok |
| osdb                |  55.8 |  196.2 |   324.8 |  1782.2 | 0.353 | 0.347 | ok |
| reymont             |  36.8 |  154.4 |   234.6 |  1331.8 | 0.309 | 0.303 | ok |
| samba               |  54.4 |  207.1 |   235.5 |  1761.8 | 0.240 | 0.241 | ok |
| sao                 |  43.1 |  162.7 |   259.2 |  1043.1 | 0.767 | 0.750 | ok |
| synth_random.bin    |  87.6 |  280.9 | 50699.3 | 72944.4 | 1.000 | 1.000 | ok |
| synth_repetitive.bin| 490.9 |  236.4 |  1478.7 | 13379.0 | 0.001 | 0.001 | ok |
| synth_zeros.bin     | 300.2 |  240.8 |  1454.9 |  3934.6 | 0.001 | 0.001 | ok |

## Summary

**Ratio — at parity.** On real data our compressed size tracks Apple's to within
about 1–3 % (e.g. dickens 0.391 vs 0.379, samba 0.240 vs 0.241, nci 0.099 vs 0.096).
Apple is fractionally tighter on most files; we are level on synthetic data. The
entropy stage (FSE) and the L/M/D model are faithful — the small gap is in match
selection, not coding.

**Speed — this is where we lag.** On compression we run at roughly **¼–⅓** of the
reference (dickens 29 vs 115 MB/s); on decompression at roughly **⅛** (dickens 158
vs 1234 MB/s). We *beat* the reference only on the degenerate repetitive/zeros inputs,
where our match-finder emits very few, very long matches and Apple's fixed per-block
overhead dominates. That is not representative.

**Correctness — solid (and three real bugs were fixed building this).** Every file in
the corpus, including 8 MiB synthetic inputs, round-trips byte-for-byte. Establishing
that surfaced and fixed three encoder/decoder defects the prior ≤128 KiB test suite
never reached:

1. **Cross-block back-references** — the decoder rebuilt each block in a fresh buffer,
   so a match referencing output from an earlier block failed with
   *"invalid match distance"* on any input above ~½ MiB. The decoder now resolves match
   distances against the whole decompressed stream.
2. **20-bit `nLiterals` overflow** — a single V2 block carrying > 2²⁰ literals silently
   corrupted its header (this triggered around 2.5 MiB of mixed data). The block splitter
   now caps each block at 512 KiB of raw bytes, keeping the 20-bit fields in range.
3. **Incompressible fallback** — random / already-compressed data was emitted as a
   broken compressed block that failed to decode. It is now stored in an uncompressed
   block, matching reference LZFSE.

## Root cause of the speed gap

- **Compression:** our match-finder is a straightforward single-cell hash chain in
  portable Go; Apple's encoder is a tuned C hash-table with a wider, history-aware
  search and far less per-byte overhead. The FSE encode also does work per symbol that
  C does with table lookups the compiler vectorises.
- **Decompression:** Apple's decoder is a tight C loop with SIMD-friendly literal/match
  copies and a branch-light FSE reader; ours is a portable Go loop appending to a slice
  with a byte-at-a-time match copy.

## Action items (to close the gap, in priority order)

1. **Decompression match copy** — the byte-at-a-time `out[matchStart+k]` loop is the
   single hottest path. Replace with an overlap-aware bulk copy (`copy`/`copyOverlap`),
   then a `go-asmgen` SIMD copy kernel on the 6 targets. Expect the biggest single win.
2. **FSE reader/writer** — hoist the bit accumulator into registers, decode L/M/D with
   fewer branches, and batch the 4 interleaved literal streams. Mirror the matchlen
   approach already used by `go-compressions/lz4`.
3. **Match-finder** — widen the hash table and add lazy matching (the lever
   `go-compressions/lz4` already uses to *beat* its reference on ratio) to both close
   the remaining ratio gap and reduce confirm overhead.
4. **Byte-compatibility with Apple's stream** — see below; a prerequisite for using our
   encoder as a drop-in for `.lzfse` artifacts consumed by Apple tooling.
5. **Parallel blocks** — blocks are independent; a worker pool would give near-linear
   multi-core compression. Tracked separately from the single-core parity bar.

## Cross-compatibility (known limitation)

Our V2 FSE bitstream layout is **not yet byte-identical** to Apple's: a stream we
produce does not decode with Apple's `lzfse` CLI, and vice versa, despite both using the
`bvx2` block magic. Round-trip within our own codec is correct. Achieving bit-exact
interop with the reference bitstream is action item 4 above and is required before we can
claim "byte-compatible with Apple's lzfse".

## Reproduce

```sh
cd benchmarks
./run.sh                       # builds reference -O3, fetches corpus, runs
# or, against an existing corpus + prebuilt C harness:
go run . -corpus ./corpus -ref ./ref/lzfse_bench -iters 21
```

`benchmarks/` is a separate Go module, so it is excluded from the library's
`go test ./...` run and 100 % coverage gate.
