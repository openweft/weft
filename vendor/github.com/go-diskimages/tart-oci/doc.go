// Package tartoci pulls Tart virtual-machine images from an OCI registry and
// materializes their contents (raw disk image, VM config, NVRAM).
//
// Tart (https://github.com/cirruslabs/tart) stores a VM image as an OCI
// artifact whose layers carry the machine's parts:
//
//   - application/vnd.cirruslabs.tart.config.v1 — the VM config (JSON).
//   - application/vnd.cirruslabs.tart.disk.v2   — the disk, split into layers of
//     at most 512 MiB (uncompressed) each compressed as an Apple LZ4 *frame*
//     (bv41/bv4-/bv4$ blocks with a shared 64 KiB window — decoded by
//     github.com/go-compressions/lz4). Every disk layer carries the annotations
//     org.cirruslabs.tart.uncompressed-size and
//     org.cirruslabs.tart.uncompressed-content-digest.
//   - application/vnd.cirruslabs.tart.nvram.v1  — the NVRAM store (raw bytes).
//
// The registry client speaks the OCI distribution (registry v2) API — GET
// /v2/<name>/manifests/<ref> and /v2/<name>/blobs/<digest> — with the anonymous
// Bearer-token flow used by ghcr.io and Docker Hub. Every blob is verified
// against its sha256 digest, and every disk layer additionally against its
// declared uncompressed size and content digest, before its bytes are trusted.
//
// The layout on which this package was grounded was captured from the real
// public image ghcr.io/cirruslabs/macos-sequoia-base; a real disk layer decodes
// byte-for-byte to the length and SHA-256 recorded in its OCI annotations.
//
// The whole pull/verify/decompress path is pure Go (CGO=0) and portable across
// every Go target: there is no Virtualization.framework or other darwin
// dependency here — only bytes.
package tartoci
