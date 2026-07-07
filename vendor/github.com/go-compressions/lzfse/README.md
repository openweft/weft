<p align="center"><img src="https://raw.githubusercontent.com/go-compressions/brand/main/social/go-compressions-lzfse.png" alt="go-compressions/lzfse" width="720"></p>

# lzfse

[![ci](https://github.com/go-compressions/lzfse/actions/workflows/ci.yml/badge.svg)](https://github.com/go-compressions/lzfse/actions/workflows/ci.yml)
![coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)

Pure-Go implementation of Apple's **LZFSE** and **LZVN** compression formats.

**Interoperability status** (verified on macOS against the reference `liblzfse`
C library and Apple's system `libcompression`, which emit byte-identical
streams; both directions are exercised by the `macos-latest` CI job):

- **All block formats are wire-interoperable both ways** — stored (`bvx-`), LZVN
  (`bvxn`), and **LZFSE (`bvx2`)**. A reference/Apple stream of any kind
  round-trips byte-for-byte through `Decompress`, and the output of `Compress`
  decodes byte-for-byte with `liblzfse` / `libcompression`, across sizes from a
  single byte through multi-block streams (700 KB+).
- Genuinely-malformed `bvx2` input (truncated/over-long freq tables, bad block
  lengths, out-of-range FSE states) is **rejected with an error** rather than
  silently decoded. (LZFSE has no per-block checksum, so a corrupted *payload*
  bit decodes to wrong-but-valid-length output — exactly as in the reference,
  byte-for-byte; that is inherent to the format, not a defect here.)

The compressed output is **not byte-identical** to the reference encoder (a
different but fully valid — and decodable — encoding of the same data).

## Module

```text
github.com/go-compressions/lzfse
```

## API

```go
func Compress(src []byte) ([]byte, error)
func Decompress(src []byte) ([]byte, error)
```

`Compress` picks the format automatically: inputs ≤ 4 KiB are emitted as an
LZVN block (one `bvxn` block followed by `bvx$` end-of-stream); larger inputs
are emitted as LZFSE blocks (V1/V2 headers + FSE-encoded streams).
`Decompress` recognises every block magic Apple's reference emits and decodes
both its own streams and reference/Apple streams of every kind, byte-for-byte
(see the interoperability status above and [BENCHMARKS.md](BENCHMARKS.md)):

| Magic  | Bytes  | Meaning                            | Reference interop      |
| ------ | ------ | ---------------------------------- | ---------------------- |
| `bvx-` | `2D…`  | Uncompressed payload (passthrough) | yes                    |
| `bvx1` | `31…`  | LZFSE V1 (uncompressed freq table) | yes (decode)           |
| `bvx2` | `32…`  | LZFSE V2 (variable-length codes)   | yes (both directions)  |
| `bvxn` | `6E…`  | LZVN block                         | yes                    |
| `bvx$` | `24…`  | End-of-stream marker               | yes                    |

## Usage

```go
import "github.com/go-compressions/lzfse"

compressed, err := lzfse.Compress(payload)
if err != nil { /* ... */ }

decoded, err := lzfse.Decompress(compressed)
if err != nil { /* ... */ }
```

## Consumers

- `github.com/go-compressions/lzfsec` — CLI wrapper (`lzfsec compress|decompress`).
- `github.com/go-filesystems/apfs` — APFS `decmpfs` transparent decompression for
  types 7 / 8 / 11 / 12 (LZVN / LZFSE, inline + resource-fork variants).
- `github.com/go-diskimages/tart-oci` — Tart layer decompression.

## Development

The package ships a [Taskfile](https://taskfile.dev) for the common build,
test, and lint targets used by both local development and the GitHub Actions
workflow at [.github/workflows/ci.yml](.github/workflows/ci.yml).

```sh
task lint    # go vet
task build   # go build
task test    # go test -race + coverage report
task ci      # lint + build + test, what CI runs
```

Dependency updates are handled by Renovate ([renovate.json](renovate.json));
patch and minor `gomod` updates auto-merge.

## License

[BSD 3-Clause](LICENSE).

## Test coverage

`task test` reports **100 % statement coverage** ([`cover.out`](cover.out)).
The corruption / random-garbage fuzz suites assert no-panic, so the
decoder is safe to call on adversarial input — bad data returns an
error rather than crashing.
