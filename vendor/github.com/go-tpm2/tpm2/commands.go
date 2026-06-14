// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import "github.com/go-tpm2/common"

// Startup runs TPM2_Startup, which the platform firmware issues exactly
// once after _TPM_Init to bring the TPM out of reset. su is a TPM_SU:
// common.SUClear for Startup(CLEAR) or common.SUState for Startup(STATE).
//
// Wire: TagNoSessions, CC 0x00000144, parameter TPM_SU startupType (u16).
// TCG "TPM 2.0 Part 3: Commands", clause "TPM2_Startup".
func (tpm *TPM) Startup(su uint16) error {
	params := common.PutU16(nil, su)
	_, err := tpm.execute(common.TagNoSessions, common.CCStartup, params)
	return err
}

// Shutdown runs TPM2_Shutdown, preparing the TPM for power loss. su is a
// TPM_SU: common.SUClear or common.SUState.
//
// Wire: TagNoSessions, CC 0x00000145, parameter TPM_SU shutdownType (u16).
// TCG "TPM 2.0 Part 3: Commands", clause "TPM2_Shutdown".
func (tpm *TPM) Shutdown(su uint16) error {
	params := common.PutU16(nil, su)
	_, err := tpm.execute(common.TagNoSessions, common.CCShutdown, params)
	return err
}

// SelfTest runs TPM2_SelfTest. When full is true (TPMI_YES_NO == YES) the
// TPM tests every implemented function; when false it tests only those not
// yet tested since the last reset.
//
// Wire: TagNoSessions, CC 0x00000143, parameter TPMI_YES_NO fullTest (u8;
// YES = 1, NO = 0). TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_SelfTest"; TPMI_YES_NO per "Part 2: Structures".
func (tpm *TPM) SelfTest(full bool) error {
	params := common.PutU8(nil, yesNo(full))
	_, err := tpm.execute(common.TagNoSessions, common.CCSelfTest, params)
	return err
}

// GetRandom runs TPM2_GetRandom and returns up to n bytes from the TPM's
// random number generator. The TPM may return fewer than n bytes.
//
// Wire request: TagNoSessions, CC 0x0000017B, parameter UINT16
// bytesRequested. Wire response: TPM2B_DIGEST randomBytes (a u16 size
// prefix followed by the bytes). TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_GetRandom".
func (tpm *TPM) GetRandom(n uint16) ([]byte, error) {
	params := common.PutU16(nil, n)
	rp, err := tpm.execute(common.TagNoSessions, common.CCGetRandom, params)
	if err != nil {
		return nil, err
	}
	val, _, err := common.UnmarshalTPM2B(rp)
	if err != nil {
		return nil, err
	}
	// Copy out of the response alias so the caller owns the bytes.
	out := make([]byte, len(val))
	copy(out, val)
	return out, nil
}

// GetCapability runs TPM2_GetCapability. cap is a TPM_CAP selector, prop is
// the first property to return, and count caps how many values to return.
// It reports moreData (whether the TPM has further values beyond this
// reply) and the raw TPMS_CAPABILITY_DATA blob.
//
// The TPMS_CAPABILITY_DATA union is large and capability-specific; decoding
// it fully is a deliberate follow-up. The documented boundary here is: the
// returned data is everything after the one-byte moreData flag, i.e. the
// complete TPMS_CAPABILITY_DATA { capability:u32, data:union } as the TPM
// sent it, ready to be parsed by a capability-aware caller.
//
// Wire request: TagNoSessions, CC 0x0000017A, parameters TPM_CAP capability
// (u32), UINT32 property, UINT32 propertyCount. Wire response: TPMI_YES_NO
// moreData (u8) followed by TPMS_CAPABILITY_DATA. TCG "TPM 2.0 Part 3:
// Commands", clause "TPM2_GetCapability".
func (tpm *TPM) GetCapability(cap, prop, count uint32) (more bool, data []byte, err error) {
	params := common.PutU32(nil, cap)
	params = common.PutU32(params, prop)
	params = common.PutU32(params, count)
	rp, err := tpm.execute(common.TagNoSessions, common.CCGetCapability, params)
	if err != nil {
		return false, nil, err
	}
	flag, ok := common.GetU8(rp, 0)
	if !ok {
		return false, nil, common.ErrShortBuffer
	}
	blob := rp[1:]
	out := make([]byte, len(blob))
	copy(out, blob)
	return flag != 0, out, nil
}

// PCRRead runs TPM2_PCR_Read for the banks/PCRs named in sel. It returns
// the PCR update counter, and the digests in the order the TPM returns
// them (which follows the selection's ascending PCR order, possibly split
// across multiple replies when more PCRs are selected than fit in one
// response — this starter reads exactly one reply).
//
// Wire request: TagNoSessions, CC 0x0000017E, parameter TPML_PCR_SELECTION
// pcrSelectionIn. Wire response: UINT32 pcrUpdateCounter, TPML_PCR_SELECTION
// pcrSelectionOut (echoed; skipped here), TPML_DIGEST pcrValues (a u32 count
// followed by that many TPM2B_DIGEST). TCG "TPM 2.0 Part 3: Commands",
// clause "TPM2_PCR_Read".
func (tpm *TPM) PCRRead(sel []PCRSelection) (updateCounter uint32, digests [][]byte, err error) {
	params := marshalPCRSelectionList(sel)
	rp, err := tpm.execute(common.TagNoSessions, common.CCPCRRead, params)
	if err != nil {
		return 0, nil, err
	}
	updateCounter, ok := common.GetU32(rp, 0)
	if !ok {
		return 0, nil, common.ErrShortBuffer
	}
	// Skip the echoed pcrSelectionOut.
	_, rest, err := parsePCRSelectionList(rp[4:])
	if err != nil {
		return 0, nil, err
	}
	// TPML_DIGEST: count then that many TPM2B_DIGEST.
	count, ok := common.GetU32(rest, 0)
	if !ok {
		return 0, nil, common.ErrShortBuffer
	}
	rest = rest[4:]
	for i := uint32(0); i < count; i++ {
		val, after, derr := common.UnmarshalTPM2B(rest)
		if derr != nil {
			return 0, nil, derr
		}
		d := make([]byte, len(val))
		copy(d, val)
		digests = append(digests, d)
		rest = after
	}
	return updateCounter, digests, nil
}

// PCRExtend runs TPM2_PCR_Extend, folding digest into PCR pcr in the bank
// named by hash (a TPM_ALG_ID). It authorizes the operation with a
// cleartext password session over the empty authValue — the standard way
// to extend an unowned platform PCR.
//
// This is the one command in the starter set that carries an authorization
// area, so the body is laid out in three regions (TCG "TPM 2.0 Part 1:
// Architecture", "Command Authorization Area Structure"):
//
//	handle area: pcrHandle (u32)                          — the PCR being extended
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND
//	param  area: TPML_DIGEST_VALUES                        — { count, {hashAlg, digest} }
//
// The TPMS_AUTH_COMMAND is the password session:
//
//	sessionHandle      = TPM_RS_PW (0x40000009)
//	nonce              = empty TPM2B               -> 0x0000
//	sessionAttributes  = continueSession (0x01)
//	hmac               = empty TPM2B               -> 0x0000
//
// which is 9 bytes on the wire (4 sessionHandle + 2 empty-nonce size + 1
// attributes + 2 empty-hmac size), so authorizationSize is 0x00000009. The
// 0x00 high byte of TPM_HT_PCR (the PCR handle type) is INFERRED from
// "Part 2: Structures", "TPM_HT (Handle Types)": PCR handles occupy
// 0x00000000..0x0000001F, so PCR[n] marshals as the bare index n.
//
// Wire: TagSessions, CC 0x00000182. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_PCR_Extend"; TPML_DIGEST_VALUES / TPMT_HA per "Part 2: Structures".
func (tpm *TPM) PCRExtend(pcr int, hash uint16, digest []byte) error {
	// Handle area: the PCR handle. PCR[n] is the permanent handle n
	// (TPM_HT_PCR == 0x00), so the handle value is simply n.
	body := common.PutU32(nil, uint32(pcr))

	// Auth area: a TPMS_AUTH_COMMAND for the empty-auth password session,
	// length-prefixed by authorizationSize.
	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	// Parameter area: TPML_DIGEST_VALUES with a single TPMT_HA.
	body = common.PutU32(body, 1) // count
	body = common.PutU16(body, hash)
	body = append(body, digest...)

	_, err := tpm.execute(common.TagSessions, common.CCPCRExtend, body)
	return err
}

// marshalPasswordAuth renders the TPMS_AUTH_COMMAND for a cleartext
// password authorization over the empty authValue:
//
//	[ sessionHandle:u32 = TPM_RS_PW
//	| nonce:TPM2B       = 0x0000      (empty)
//	| sessionAttributes:u8 = 0x01     (continueSession)
//	| hmac:TPM2B        = 0x0000      (empty) ]
//
// The on-wire length is 4 + 2 + 1 + 2 = 9 bytes. TCG "TPM 2.0 Part 2:
// Structures", clause "TPMS_AUTH_COMMAND"; "Part 1: Architecture",
// "Password Authorizations".
func marshalPasswordAuth() []byte {
	out := common.PutU32(nil, TPM_RS_PW)     // sessionHandle
	out = common.PutU16(out, 0)              // nonce: empty TPM2B (size 0)
	out = common.PutU8(out, sessionContinue) // sessionAttributes
	out = common.PutU16(out, 0)              // hmac: empty TPM2B (size 0)
	return out
}

// sessionContinue is the continueSession bit of TPMA_SESSION (bit 0): the
// TPM retains the session for the next command rather than flushing it.
// TCG "TPM 2.0 Part 2: Structures", clause "TPMA_SESSION".
const sessionContinue = 0x01

// yesNo maps a Go bool to the TPMI_YES_NO wire byte (YES = 1, NO = 0).
func yesNo(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
