// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/common authors. All rights reserved.

package common

// Transport is the contract a concrete TPM transport (CRB, TIS,
// passthrough to a host /dev/tpm0, or a TCP socket to a software TPM)
// implements, and that the tpm2 command layer consumes.
//
// It is deliberately minimal: the command layer marshals a complete TPM
// 2.0 command buffer (header + parameters, as produced by BuildCommand),
// hands it to Send, and receives the complete response buffer (header +
// parameters) back. Framing, MMIO register handshakes, locality, and
// retry are entirely the transport's concern and never leak into this
// interface.
type Transport interface {
	// Send transmits one fully-marshaled TPM command buffer and returns
	// the full response buffer. The returned buffer includes the 10-byte
	// TPM 2.0 response header and is suitable for ParseResponse.
	Send(cmd []byte) (rsp []byte, err error)
}
