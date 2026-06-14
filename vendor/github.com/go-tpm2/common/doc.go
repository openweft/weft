// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/common authors. All rights reserved.

// Package common holds the transport-agnostic, platform-agnostic
// foundation shared by every layer of the pure-Go TPM 2.0 stack in the
// go-tpm2 family. It mirrors the role of go-virtio/common: it owns the
// small plug-in interfaces, the big-endian TPM 2.0 wire codec, and the
// spec-derived constants, while the higher layers (the tpm2 command
// layer) and the lower layers (the crb/tis/passthrough/socket
// transports) sit on either side and import this package.
//
// Two interfaces stitch the stack together:
//
//   - Transport: the contract a transport implements and the tpm2
//     command layer consumes. It exchanges one fully-marshaled TPM
//     command buffer for the full response buffer (see transport.go).
//
//   - Regs: the platform-provided MMIO accessor that the register-level
//     drivers (CRB, TIS) use to touch the TPM register window. It is the
//     analogue of go-virtio's BAR accessor (see regs.go).
//
// The wire codec (codec.go) builds and parses the 10-byte TPM 2.0
// command/response header and the ubiquitous TPM2B size-prefixed blob.
//
// Wire format note: the TPM 2.0 wire encoding is BIG-ENDIAN throughout
// (TCG "TPM 2.0 Part 1: Architecture", clause "Data Marshaling" /
// "Canonicalization"). Every multi-byte field in this package is encoded
// most-significant-byte first.
//
// Conventions: pure Go, CGO_ENABLED=0, no architecture-specific
// assembly, BSD-3-Clause on every file, 100% statement coverage, and
// GOWORK=off (the module is not part of any workspace).
package common
