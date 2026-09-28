# Performance parity — go-simd/matchlen vs scalar baseline

**Methodology.** Apple M4 Max (arm64, NEON), macOS (Darwin 25.5.0), Go 1.26.4,
single core. `MatchLen` returns the length of the common prefix of two byte
slices — the LZ-family match-finder primitive. The fair reference is the naive
scalar byte-at-a-time compare loop (`refScalar`) every compressor uses without
SIMD; go-simd runs a NEON wide-compare kernel (go-asmgen) plus a scalar tail.
Inputs share a **full-length common prefix with the mismatch on the last byte**
(worst case — the loop runs the entire length), seed 2, sizes 16 B … 64 KiB;
`-benchtime=0.3s -count=3`, median reported. Correctness: `go test` (incl. fuzz)
matches `refScalar` on every input before benchmarking. Reproduce:

```
GOWORK=off go test -run='^$' -bench=Parity -benchmem -benchtime=0.3s -count=3 .
```

go-simd/matchlen has NEON on arm64, so these are real SIMD numbers.

| op | match len | go-simd (GB/s) | scalar baseline (GB/s) | speedup vs scalar | verdict |
|----|-----------|---------------:|-----------------------:|------------------:|---------|
| MatchLen | 16 B   |  5.71 | 3.07 |  1.86× | beats scalar (tail-dominated) |
| MatchLen | 64 B   | 18.86 | 3.45 |  5.47× | strong win |
| MatchLen | 256 B  | 34.39 | 2.87 | 11.98× | strong win |
| MatchLen | 4 KiB  | 32.48 | 3.49 |  9.31× | strong win |
| MatchLen | 64 KiB | 38.69 | 3.51 | 11.02× | **strong win** |

## amd64 (AVX2, GitHub Actions x86_64 runner — ratios valid, absolute ns/op CI-noisy)

**Methodology.** GitHub Actions `ubuntu-latest` runner, **AMD EPYC 7763** (`avx2`
present, **no `avx512*`** — confirmed from `/proc/cpuinfo`), `GOAMD64` baseline,
Go stable, single core. Same parity harness, `-count=6`, **min-of-6**. The runner
is shared, so absolute throughput is noisy and **not comparable to the arm64 M4
Max rows above** (different hardware/ISA); the **ratio vs the scalar baseline**
is measured back-to-back on the *same* CPU and is valid. Reproduce via
`gh workflow run bench-amd64.yml`.

| match len | go-simd (MB/s) | scalar baseline | ×scalar | verdict |
|-----------|---------------:|----------------:|--------:|---------|
| 16 B   |  1220 | 1426 |  0.86× | trails scalar (sub-stride, tail-dominated) |
| 64 B   |  8202 | 1368 |  6.00× | strong win |
| 256 B  | 24122 | 1535 | 15.71× | strong win |
| 4 KiB  | 36560 | 1599 | 22.86× | strong win |
| 64 KiB | 42319 | 1603 | 26.40× | **strong win** |

* **Beats the scalar baseline 6–26×** for matches ≥ 64 B (even larger margins
  than arm64). Zero allocations.
* **Honest finding (amd64):** at **16 B the AVX2 path trails scalar (0.86×)** —
  below one vector the SIMD setup/tail dominates and the scalar byte loop wins on
  sub-stride matches (on arm64 this case still nets 1.9×; on amd64 it dips just
  below parity). Reinforces action item 2 (lower the SIMD threshold for 16–32 B).

## Summary

* **Beats the scalar baseline 5.5–12×** for matches ≥ 64 B, sustaining
  ~33–39 GB/s vs the scalar loop's ~3.5 GB/s. Zero allocations.
* At 16 B the win is only 1.9× — the SIMD body covers one vector and the scalar
  tail dominates; expected and acceptable for sub-stride matches.
* There is no widely-used standalone Go reference library for this exact
  primitive (it lives inside each compressor, e.g. pierrec/lz4's internal
  `matchLen`); the scalar baseline is the correct fair reference.

### Action items
1. ~~**amd64/AVX2 follow-up:** measure the AVX2 kernel on a real x86_64 VM.~~
   **Done** (see the amd64 section) — on the GitHub Actions x86_64 runner (EPYC
   7763, AVX2) the AVX2 kernel beats the scalar baseline **6–26×** for ≥ 64 B; the
   16 B sub-stride case dips to 0.86×.
2. Consider lowering the SIMD threshold / unrolling the 16–32 B range so short
   matches (very common in real LZ streams) get more of the SIMD win.
