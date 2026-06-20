<p align="center"><img src="https://raw.githubusercontent.com/go-compressions/brand/main/social/go-compressions-lzfse.png" alt="go-compressions/lzfse" width="720"></p>

# lzfse

[![ci](https://github.com/go-compressions/lzfse/actions/workflows/ci.yml/badge.svg)](https://github.com/go-compressions/lzfse/actions/workflows/ci.yml)
![coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)

Pure-Go implementation of Apple's **LZFSE** and **LZVN** compression formats.
Byte-compatible with the reference `liblzfse` C implementation: data compressed
by `liblzfse` round-trips through `Decompress`, and data produced by `Compress`
is decoded by `liblzfse` without modification.

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
`Decompress` handles every block magic Apple's reference emits:

| Magic  | Bytes  | Meaning                            |
| ------ | ------ | ---------------------------------- |
| `bvx-` | `2D…`  | Uncompressed payload (passthrough) |
| `bvx1` | `31…`  | LZFSE V1 (uncompressed freq table) |
| `bvx2` | `32…`  | LZFSE V2 (variable-length codes)   |
| `bvxn` | `6E…`  | LZVN block                         |
| `bvx$` | `24…`  | End-of-stream marker               |

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
