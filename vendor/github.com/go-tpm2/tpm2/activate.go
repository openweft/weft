// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import "github.com/go-tpm2/common"

// This file implements the ATTESTING platform's side of Credential
// Protection: TPM2_ActivateCredential, plus the TPM2_PolicySecret needed to
// satisfy the Endorsement Key's well-known policy and the multi-session
// authorization area the command requires. ActivateCredential proves the AK
// and the EK live on the SAME TPM: only that TPM can run the EK's private
// ECDH to recover the seed and thus the credential MakeCredential bound to
// the AK's Name. TCG "TPM 2.0 Part 3: Commands", clauses
// "TPM2_ActivateCredential" and "TPM2_PolicySecret"; "Part 1: Architecture",
// "Credential Protection" and "Enhanced Authorization".

// Command codes for the credential-activation flow. common v0.1.0 predates
// this milestone, so they are defined here with the TCG-registered TPM_CC
// values. TCG "TPM 2.0 Part 2: Structures", clause "TPM_CC (Command Codes)".
const (
	// ccActivateCredential is TPM2_ActivateCredential.
	ccActivateCredential common.TPM_CC = 0x00000147
	// ccPolicySecret is TPM2_PolicySecret.
	ccPolicySecret common.TPM_CC = 0x00000151
)

// PolicySecret runs TPM2_PolicySecret in policySession, asserting knowledge of
// authHandle's authValue. For the EK policy, authHandle is TPM_RH_ENDORSEMENT
// authorized with the empty endorsement password (RS_PW), and the call
// extends the session's policyDigest by the PolicySecret assertion. With
// empty nonceTPM, cpHashA, policyRef and expiration=0, the resulting
// policyDigest equals the well-known EK authPolicy
// (= policyDigest of TPM2_PolicySecret(TPM_RH_ENDORSEMENT)).
//
// Body (TagSessions):
//
//	handle area: authHandle (u32) = TPM_RH_ENDORSEMENT
//	             policySession (u32)              — second handle, NO auth area
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_NONCE nonceTPM (empty) || TPM2B_DIGEST cpHashA (empty) ||
//	             TPM2B_NONCE policyRef (empty) || INT32 expiration (0)
//
// Only authHandle requires authorization, so the auth area carries exactly
// ONE TPMS_AUTH_COMMAND (for authHandle); policySession sits in the handle
// area without an auth entry. TCG "Part 3", "TPM2_PolicySecret".
//
// Response (TagSessions): parameterSize (u32) || TPM2B_TIMEOUT timeout ||
// TPMT_TK_AUTH policyTicket. Neither is needed when expiration is 0, so they
// are not parsed.
func (tpm *TPM) PolicySecret(authHandle, policySession uint32) error {
	body := common.PutU32(nil, authHandle)    // authHandle (with auth)
	body = common.PutU32(body, policySession) // policySession (no auth)

	auth := marshalPasswordAuth() // RS_PW, empty endorsement auth
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = common.PutU16(body, 0) // nonceTPM: empty TPM2B
	body = common.PutU16(body, 0) // cpHashA: empty TPM2B
	body = common.PutU16(body, 0) // policyRef: empty TPM2B
	body = common.PutU32(body, 0) // expiration: INT32 0

	_, err := tpm.execute(common.TagSessions, ccPolicySecret, body)
	return err
}

// marshalAuthArea concatenates one or more already-marshaled TPMS_AUTH_COMMAND
// sessions into a complete command authorization area: an authorizationSize
// (UINT32) covering the joined sessions, followed by the sessions in HANDLE
// order. A command with N auth-requiring handles carries N sessions here, in
// the same order the handles appear in the handle area. TCG "TPM 2.0 Part 1:
// Architecture", "Command Authorization Area"; "Part 2", "TPMS_AUTH_COMMAND".
func marshalAuthArea(sessions ...[]byte) []byte {
	var joined []byte
	for _, s := range sessions {
		joined = append(joined, s...)
	}
	out := common.PutU32(nil, uint32(len(joined)))
	return append(out, joined...)
}

// ActivateCredential runs TPM2_ActivateCredential. activateHandle is the AK
// (authorized with the empty RS_PW password, since the AK has userWithAuth);
// keyHandle is the EK (authorized with a POLICY SESSION that already satisfies
// the EK policy via PolicySecret). credentialBlob is the TPM2B_ID_OBJECT and
// secret the TPM2B_ENCRYPTED_SECRET produced by MakeCredential — each passed
// as its complete, already-TPM2B-wrapped wire form (i.e. the
// MakeCredentialResult fields verbatim). The TPM unwraps them and returns
// certInfo: the recovered credential (a TPM2B_DIGEST), which equals the
// original iff the AK Name and EK match.
//
// Body (TagSessions) — note TWO handles and TWO sessions:
//
//	handle area: activateHandle (u32) = AK || keyHandle (u32) = EK
//	auth   area: authorizationSize (u32) ||
//	             TPMS_AUTH_COMMAND #1 (AK:  RS_PW, empty) ||
//	             TPMS_AUTH_COMMAND #2 (EK:  policy session)
//	param  area: TPM2B_ID_OBJECT credentialBlob || TPM2B_ENCRYPTED_SECRET secret
//
// The two sessions are in handle order: the AK's password session first
// (activateHandle is handle #1), the EK's policy session second (keyHandle is
// handle #2). TCG "Part 3", "TPM2_ActivateCredential".
//
// Response (TagSessions): parameterSize (u32) || TPM2B_DIGEST certInfo.
func (tpm *TPM) ActivateCredential(activateHandle, keyHandle, policySession uint32, credentialBlob, secret []byte) ([]byte, error) {
	body := common.PutU32(nil, activateHandle) // handle #1: AK
	body = common.PutU32(body, keyHandle)      // handle #2: EK

	akAuth := marshalPasswordAuth()            // AK: RS_PW empty
	ekAuth := marshalPolicyAuth(policySession) // EK: policy session
	body = append(body, marshalAuthArea(akAuth, ekAuth)...)

	body = append(body, credentialBlob...) // TPM2B_ID_OBJECT (already wrapped)
	body = append(body, secret...)         // TPM2B_ENCRYPTED_SECRET (already wrapped)

	rp, err := tpm.execute(common.TagSessions, ccActivateCredential, body)
	if err != nil {
		return nil, err
	}
	// parameterSize (TagSessions response).
	if _, ok := common.GetU32(rp, 0); !ok {
		return nil, common.ErrShortBuffer
	}
	certInfo, _, err := common.UnmarshalTPM2B(rp[4:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(certInfo))
	copy(out, certInfo)
	return out, nil
}

// ActivateAKWithEK ties the whole credential-activation flow together. It
// computes MakeCredential OFF the TPM from ekPublic and the AK's Name (derived
// from akPublic), opens a policy session and satisfies the EK policy with
// PolicySecret(TPM_RH_ENDORSEMENT), then runs TPM2_ActivateCredential with the
// AK as activateHandle and the EK as keyHandle. It returns the recovered
// credential, which a caller asserts equals the original challenge — proving
// the AK and EK are on the same TPM.
//
// ak and ek are the loaded transient handles; akPublic and ekPublic are the
// corresponding TPMT_PUBLIC bytes (the contents of TPM2B_PUBLIC, used to
// derive the AK Name and the EK point). credential is the challenge to bind.
// nonceCaller is the 32-byte caller nonce for the policy session.
func (tpm *TPM) ActivateAKWithEK(ak, ek uint32, akPublic, ekPublic, credential, nonceCaller []byte) ([]byte, error) {
	akName, err := ObjectName(akPublic)
	if err != nil {
		return nil, err
	}
	ekPoint, err := parseTPMTPublicECCPoint(ekPublic)
	if err != nil {
		return nil, err
	}
	mc, err := MakeCredential(EKPublic{X: ekPoint.X, Y: ekPoint.Y}, akName, credential, nil)
	if err != nil {
		return nil, err
	}

	session, _, err := tpm.StartAuthSession(nonceCaller)
	if err != nil {
		return nil, err
	}
	if err := tpm.PolicySecret(RHEndorsement, session); err != nil {
		return nil, err
	}
	// MakeCredentialResult already carries the complete TPM2B_ID_OBJECT and
	// TPM2B_ENCRYPTED_SECRET wire forms, which ActivateCredential appends
	// verbatim into the parameter area.
	return tpm.ActivateCredential(ak, ek, session, mc.CredentialBlob, mc.Secret)
}
