// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/devtpm authors. All rights reserved.

// Package devtpm implements a pure-Go github.com/go-tpm2/common.Transport
// over a Linux kernel TPM character device — by default /dev/tpmrm0, the
// in-kernel TPM2 resource-manager channel.
//
// # Device model
//
// The Linux tpm subsystem (drivers/char/tpm) exposes a TPM through two
// character devices:
//
//   - /dev/tpm0 — the raw device: a single-open, unmediated TPM2 command
//     channel with no handle virtualization.
//   - /dev/tpmrm0 — the resource-manager device (DefaultDevice): each open
//     file descriptor is an independent, multiplexed command channel; the
//     kernel resource manager virtualizes the TPM's scarce transient-object
//     and session slots and flushes a client's transient context when the
//     descriptor is closed.
//
// Both devices share the same framing, and it is the framing this package
// depends on: a single write() delivers exactly one complete TPM 2.0
// command to the TPM, and the matching read() returns exactly one complete
// response (the driver buffers the response and returns it whole). Send
// therefore performs one Write of the full command and one Read of the
// full response — no length prefix, no chunking, no partial-write retry.
//
// Because that contract is just "raw TPM2 bytes, one command per write,
// one response per read", New also accepts any io.ReadWriteCloser that
// honors it: most usefully a unix-domain socket to swtpm's --server data
// channel, which speaks the identical raw TPM2 protocol. The validate
// harness uses exactly that to exercise this transport against a real
// swtpm on a host that has no /dev/tpmrm0.
//
// # Security and usage (weft attestation)
//
// This is the node-side host-TPM transport for weft remote attestation.
// On a real Linux node, the node agent opens the host's hardware (or
// firmware/vTPM) TPM via devtpm.Open(devtpm.DefaultDevice), layers the
// go-tpm2/tpm2 command API on top, and drives go-tpm2/attest's Node side:
// reading PCRs, loading/creating the attestation key, and producing the
// quote the verifier checks.
//
// Operational notes:
//
//   - Prefer /dev/tpmrm0. The resource manager prevents a misbehaving or
//     crashing client from leaking transient handles and wedging the
//     shared TPM, and it isolates concurrent users of the same TPM.
//   - Access to /dev/tpmrm0 is privileged (typically root or the "tss"
//     group). Any process that can open it can ask the TPM to sign with
//     keys it can use and can read every PCR; treat the descriptor as a
//     sensitive capability and keep it inside the node agent.
//   - This transport carries opaque bytes only; it performs no policy,
//     authorization, or measurement itself. The attestation semantics
//     (which PCRs, which key, freshness/nonce) live in go-tpm2/attest.
//
// # Conventions
//
// Pure Go, CGO_ENABLED=0, no architecture-specific assembly, BSD-3-Clause
// on every file, 100% statement coverage (GOWORK=off go test -cover), and
// GOWORK=off. The package consumes github.com/go-tpm2/common's Transport
// contract and HeaderSize/Error helpers and nothing else.
package devtpm
