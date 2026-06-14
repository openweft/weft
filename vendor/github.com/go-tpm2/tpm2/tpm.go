// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import (
	"fmt"

	"github.com/go-tpm2/common"
)

// TPM_RS_PW is the password-authorization session handle: the well-known
// handle that selects a cleartext-password authorization in a command's
// session area, in lieu of a real HMAC/policy session. TCG "TPM 2.0 Part 2:
// Structures", clause "TPM_RS (Reserved Handles)"; used by the password
// authorization described in "TPM 2.0 Part 1: Architecture", "Password
// Authorizations".
const TPM_RS_PW uint32 = 0x40000009

// TPMError is the typed error returned when a command completes with a
// non-success response code. It carries the raw TPM_RC reported by the
// device and the TPM_CC the command was issued under, so callers can
// distinguish, for example, a TPM_RC_VALUE from PCR_Extend from the same
// rc raised by some other command.
type TPMError struct {
	// CC is the command code the failing command was issued under.
	CC uint32
	// RC is the raw response code returned by the TPM (non-zero).
	RC uint32
}

// Error implements the error interface.
func (e *TPMError) Error() string {
	return fmt.Sprintf("tpm2: command 0x%08X failed: rc 0x%08X", e.CC, e.RC)
}

// TPM is the command-layer handle. It wraps a common.Transport and issues
// fully-marshaled TPM 2.0 commands through it. It holds no state of its own;
// concurrency and framing are the transport's concern.
type TPM struct {
	t common.Transport
}

// New returns a TPM that issues commands over t.
func New(t common.Transport) *TPM {
	return &TPM{t: t}
}

// execute marshals a command (tag, cc, params), sends it through the
// transport, parses the response header, and returns the response
// parameter bytes. A non-success response code is reported as a *TPMError
// carrying cc and the raw rc. Transport and header-parse errors are
// returned verbatim.
func (tpm *TPM) execute(tag common.TPM_ST, cc common.TPM_CC, params []byte) ([]byte, error) {
	cmd := common.BuildCommand(uint16(tag), uint32(cc), params)
	rsp, err := tpm.t.Send(cmd)
	if err != nil {
		return nil, err
	}
	_, rc, rp, err := common.ParseResponse(rsp)
	if err != nil {
		return nil, err
	}
	if rc != uint32(common.RCSuccess) {
		return nil, &TPMError{CC: uint32(cc), RC: rc}
	}
	return rp, nil
}
