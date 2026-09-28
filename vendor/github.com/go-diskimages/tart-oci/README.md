<p align="center"><img src="https://raw.githubusercontent.com/go-diskimages/brand/main/social/go-diskimages.png" alt="go-diskimages/tart-oci" width="720"></p>

# tart-oci

[![CI](https://github.com/go-diskimages/tart-oci/actions/workflows/ci.yml/badge.svg)](https://github.com/go-diskimages/tart-oci/actions/workflows/ci.yml)
![coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-diskimages/tart-oci.svg)](https://pkg.go.dev/github.com/go-diskimages/tart-oci)

Pure-Go (CGO=0) puller for **[Tart](https://github.com/cirruslabs/tart)** VM
images stored as OCI artifacts in a registry (e.g. `ghcr.io/cirruslabs/*`). It
speaks the OCI distribution (registry v2) API, verifies every blob against its
digest, decompresses the disk layers, and materializes a byte-exact raw disk —
no `tart`, no `hdiutil`, no external tools, and no darwin-only code.

## Module

```text
github.com/go-diskimages/tart-oci
```

## Tart's OCI layout

Grounded in the real public image `ghcr.io/cirruslabs/macos-sequoia-base` and in
Tart's `DiskV2.swift`. A Tart image is an OCI image manifest whose layers are:

| Media type                                    | Contents                                                        |
| --------------------------------------------- | --------------------------------------------------------------- |
| `application/vnd.cirruslabs.tart.config.v1`   | VM config (JSON)                                                 |
| `application/vnd.cirruslabs.tart.disk.v2`     | one disk chunk of ≤ 512 MiB (uncompressed), **Apple LZ4 frame** |
| `application/vnd.cirruslabs.tart.nvram.v1`    | NVRAM store (raw bytes)                                          |

Each disk layer carries `org.cirruslabs.tart.uncompressed-size` and
`org.cirruslabs.tart.uncompressed-content-digest` annotations; the chunks
concatenate, in manifest order, into the full disk.

**Compression.** Tart compresses each disk chunk with Apple's Compression
framework (`.compressed(using: .lz4)`), which emits an **Apple LZ4 *frame***
(`bv41`/`bv4-`/`bv4$` blocks sharing a 64 KiB cross-block window) — *not* a bare
LZ4 block, and *not* LZFSE. Decoding is delegated to
[`github.com/go-compressions/lz4`](https://github.com/go-compressions/lz4)
(`DecompressAppleStream`), which was validated to decode a real disk layer
byte-for-byte to the length and SHA-256 in its OCI annotations.

## API

```go
// Pull a Tart image and write its raw disk image.
err := tartoci.PullDisk(ctx,
    "ghcr.io/cirruslabs/macos-sequoia-base:latest",
    "disk.raw", os.Stdout)

// Or materialize a full VM bundle (disk.img + config.json + nvram.bin).
err := tartoci.Pull(ctx,
    "ghcr.io/cirruslabs/macos-sequoia-base:latest",
    "vm/", os.Stdout)
```

Lower-level building blocks are exported too: `ParseReference`, `NewRegistry`
(`Manifest`, blob access, token auth), the `Manifest`/`Descriptor` types and the
media-type/annotation constants.

Options: `WithHTTPClient`, `WithBaseURL`, `WithBasicAuth` (for private repos).

### Verification

Every disk layer is checked against its `sha256` blob digest, its declared
uncompressed size, and its uncompressed content digest before its bytes are
trusted; config and NVRAM blobs are checked against their digests. A
digest/size mismatch, an auth failure, or a malformed manifest is returned as an
error — none of those paths silently succeed.

## Composition with go-diskimages

`Format` structurally satisfies the `github.com/go-diskimages/interface`
`Format` interface (`Name`/`Create`/`Detect`/`ToRaw`/`Resize`) over a
locally-cached Tart **OCI layout directory**, so a pulled image composes with the
rest of the `go-diskimages` family (`raw`, `qcow2`, `dmg`, …).

## License

BSD-3-Clause. Copyright (c) 2026 The go-diskimages/tart-oci authors.
