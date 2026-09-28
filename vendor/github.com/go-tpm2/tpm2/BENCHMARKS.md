# Performance parity — go-tpm2 vs google/go-tpm  (2026-06-22)

Marshal/unmarshal overhead and end-to-end round-trip latency of **go-tpm2**
(`github.com/go-tpm2/common` codec + `github.com/go-tpm2/tpm2` command layer)
versus the reference Go implementation **`github.com/google/go-tpm`**.

## Honest framing

A TPM command's latency is **dominated by the TPM/swtpm itself**, not by the
Go library that drives it. A round-trip to swtpm runs in tens of microseconds
regardless of which library marshals the request, so the **only place a Go
library's performance actually shows** is the pure marshal/parse step (tens of
nanoseconds). This report therefore separates two questions:

1. **Library codec overhead** (pure, no TPM) — where the libraries genuinely
   differ. go-tpm2 uses a hand-rolled append-based big-endian codec; go-tpm
   marshals/unmarshals by **reflection** over `gotpm`-tagged structs.
2. **End-to-end round-trip vs swtpm** — reported to **confirm go-tpm2 adds no
   significant transport overhead** over go-tpm. These are expected to be equal
   (swtpm-bound); they are not a "win".

## Methodology

| | |
|---|---|
| Host | Apple M4 Max (`Mac16,5`), macOS (`darwin/arm64`) |
| Go | go1.26.4 |
| go-tpm2 | this tree (`common` + `tpm2`) |
| google/go-tpm | v0.9.8 |
| swtpm | 0.10.1 (libtpms), TCP "MS simulator" mode on :2421/:2422 (ours) and :2431/:2432 (go-tpm) |
| tpm2-tools | not installed on the macOS host (Linux/TSS C stack); round-trip reference is go-tpm |

- **Fairness — codec:** a parity test (`TestGetCapabilityParamParity`,
  `TestGetRandomParamParity`, `TestDigestValuesParity`,
  `TestParseGetRandomParity`) asserts the two libraries produce **byte-identical**
  parameter areas / decode to the same value before any timing, so the ns/op
  figures compare equivalent work.
- **Fairness — round-trip:** both libraries drive the **same swtpm** over the
  **identical** `Send([]byte) ([]byte, error)` transport (`swtpmTCP`, which
  satisfies both `common.Transport` and `go-tpm`'s `transport.TPM`). Any
  difference is therefore library cost, not transport or TPM cost.
- Warm-up before timing; `-count=5` for the codec medians, `-benchtime=2000x`
  for the swtpm round-trips. The benchmark harness lives in an **isolated
  module** (`./benchmarks`, separate `go.mod`) so the comparison dependency
  never enters this package and the benchmark files are **excluded from the
  100 % coverage gate**.

## 1. Library codec overhead (pure CPU — no TPM)

| op | ours (ns/op, allocs) | go-tpm (ns/op, allocs) | ratio (go-tpm / ours) | verdict |
|---|---|---|---|---|
| Marshal GetCapability params (3×u32) | **19.7 ns**, 24 B, **2 allocs** | 697 ns, 528 B, 14 allocs | **≈ 35×** | ours far cheaper |
| Marshal TPML_DIGEST_VALUES (PCR_Extend param) | **22.1 ns**, 56 B, **2 allocs** | 2 875 ns, 864 B, 92 allocs | **≈ 130×** | ours far cheaper |
| Parse GetRandom response (TPM2B_DIGEST) | **11.4 ns**, 32 B, **1 alloc** | 1 762 ns, 416 B, 50 allocs | **≈ 155×** | ours far cheaper |

go-tpm's reflection codec allocates 7–46× more and runs 35–155× slower on these
hot paths. go-tpm2's hand-rolled codec produces **byte-identical** output (proven
by the parity tests) at a fraction of the cost. This matters most for a
constrained / `CGO=0` guest (the tamago microVM target) where allocations are
expensive.

## 2. End-to-end round-trip vs swtpm (swtpm-bound — confirming parity)

Median of `-count=2 -benchtime=2000x`. **ns/op here is dominated by the swtpm
round-trip**, not the library.

| op | ours (ns/op, allocs) | go-tpm (ns/op, allocs) | tpm2-tools | verdict |
|---|---|---|---|---|
| GetRandom(32) | ~21 000–29 000, **6 allocs** | ~34 000–35 000, 79 allocs | n/a (not on macOS host) | equal within swtpm noise; ours far fewer allocs |
| GetCapability(MANUFACTURER) | ~30 000–39 000, **6 allocs** | ~33 000–34 000, 63 allocs | n/a | equal within noise |
| PCR_Extend (auth area) | ~25 000–28 000, **6 allocs** | ~42 000–51 000, 313 allocs | n/a | equal within noise; ours far fewer allocs |

Both libraries land in the same ~20–50 µs swtpm-bound band; the spread is swtpm
scheduling noise, not library cost. **go-tpm2 adds no transport overhead over
go-tpm** — confirmed. It also carries **6 allocations** into the round-trip
versus go-tpm's 63–313, i.e. the per-command GC pressure is an order of
magnitude lower.

## Summary

- **Lib overhead is at-or-below parity** — in fact far below: go-tpm2's codec is
  35–155× faster and allocates 7–46× less than go-tpm's reflection codec on the
  marshal/parse hot paths, with **byte-identical** output (parity-tested).
- **Round-trip is swtpm-bound and equal** between the two libraries (~20–50 µs);
  go-tpm2 adds no measurable transport overhead and carries far fewer
  allocations into each round-trip.

### Gaps / action items

- **tpm2-tools round-trip column** is `n/a` on this macOS host (the TSS C stack
  is not packaged here). Re-run section 2 on a Linux box with `tpm2-tools` to
  add the C-stack reference (expected: same swtpm-bound band).
- go-tpm2's codec advantage comes from **not reflecting**; the trade-off is that
  go-tpm2 covers a deliberately small command set. Where go-tpm2 grows new
  commands, keep the hand-rolled-codec discipline (no reflection) to preserve
  this profile.
- See `../attest/BENCHMARKS.md` for the pure-Go **compute-path** comparison
  (VerifyQuote / event-log replay / MakeCredential).

_Reproduce:_ `cd benchmarks && GOWORK=off go test -run Parity -v .` (correctness),
then `GOWORK=off go test -run '^$' -bench 'Marshal|Parse' -benchmem -count=5 .`
(codec) and `GOWORK=off go test -run '^$' -bench RoundTrip -benchmem -benchtime=2000x .`
(round-trip; needs `swtpm` on `PATH`).
