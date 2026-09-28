// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import (
	"fmt"
	"time"

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
	for attempt := 0; ; attempt++ {
		rsp, err := tpm.t.Send(cmd)
		if err != nil {
			return nil, err
		}
		_, rc, rp, err := common.ParseResponse(rsp)
		if err != nil {
			return nil, err
		}
		if rc == uint32(common.RCSuccess) {
			return rp, nil
		}
		// ⛔ TPM_RC_RETRY IS NOT A FAILURE. It is the TPM saying it could not
		// start the command and that the caller should send it again — a
		// warning (the 0x900 bit), not an error. Returning it as one made a
		// busy TPM indistinguishable from a broken one.
		//
		// Measured 2026-09-27: openweft/weft's swtpm attestation test failed
		// the first time it ever ran, with `command 0x00000158 failed:
		// rc 0x00000922` — TPM2_Quote against a software TPM that had not
		// finished starting. It passes on a warm TPM, which is why a laptop
		// never saw it and a cold CI runner did.
		if rc != rcRetry || attempt >= retryLimit {
			return nil, &TPMError{CC: uint32(cc), RC: rc}
		}
		// Bounded and short. An unbounded wait would turn "the TPM is busy"
		// into "the program stopped", which is worse than the error it
		// replaces; the TPM spec puts no ceiling on it, so this one is ours
		// and is stated rather than tuned.
		time.Sleep(retryBackoff << attempt)
	}
}

// TPM_RC_RETRY: RC_WARN (0x900) + 0x022. Named here because this library has
// no response-code table, so nothing could tell a caller what 0x922 meant.
// Cross-checked against google/go-tpm (`RCRetry ResponseCode = 0x922`) and
// Microsoft's reference implementation (`TPM_RC_RETRY (RC_WARN+0x022)`).
const rcRetry = 0x922

const (
	// retryLimit is how many extra attempts a retry earns. Six attempts at a
	// doubling 2ms backoff is about a quarter of a second in total, which
	// covers a self-test finishing without making a genuinely stuck TPM look
	// like a hang.
	retryLimit = 6
	// retryBackoff is the first wait; each attempt doubles it.
	retryBackoff = 2 * time.Millisecond
)
