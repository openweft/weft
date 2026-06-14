// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import (
	"crypto/sha256"

	"github.com/go-tpm2/common"
)

// This file implements the measured-boot PAYOFF: sealing a secret to a set
// of PCR values and unsealing it again only when those PCRs still hold the
// sealed-to value, enforced by the TPM via a POLICY SESSION. It introduces
// TPM 2.0 policy authorization (TCG "TPM 2.0 Part 1: Architecture",
// "Enhanced Authorization (EA)") and the commands that drive it: a storage
// parent (TPM2_CreatePrimary), TPM2_Create / TPM2_Load for the sealed
// keyedhash object, TPM2_StartAuthSession + TPM2_PolicyPCR to build the
// policy in a session, and TPM2_Unseal to release the secret.

// Command codes for the seal/unseal/policy flow. common v0.1.0 predates this
// milestone, so they are defined here (with the same TPM_CC values the TCG
// registry assigns). TCG "TPM 2.0 Part 2: Structures", clause "TPM_CC
// (Command Codes)".
const (
	// ccStartAuthSession is TPM2_StartAuthSession.
	ccStartAuthSession common.TPM_CC = 0x00000176
	// ccPolicyPCR is TPM2_PolicyPCR.
	ccPolicyPCR common.TPM_CC = 0x0000017F
	// ccUnseal is TPM2_Unseal.
	ccUnseal common.TPM_CC = 0x0000015E
	// ccPolicyGetDigest is TPM2_PolicyGetDigest.
	ccPolicyGetDigest common.TPM_CC = 0x00000189
)

// Permanent handles and selectors used by the policy flow. TCG "TPM 2.0
// Part 2: Structures", clauses "TPM_RH (Permanent Handles)" and "TPM_SE".
const (
	// rhNull is TPM_RH_NULL: used as tpmKey and bind in StartAuthSession to
	// request an unsalted, unbound session.
	rhNull uint32 = 0x40000007
	// sePolicy is TPM_SE_POLICY: a policy session (one that accumulates a
	// policyDigest via TPM2_Policy* commands). TCG "Part 2", clause "TPM_SE".
	sePolicy uint8 = 0x01
)

// Object-type and KEYEDHASH algorithm ids used by the sealed object. TCG
// "TPM 2.0 Part 2: Structures", clause "TPM_ALG_ID".
const (
	// algKeyedHash is TPM_ALG_KEYEDHASH (0x0008): the object type of a sealed
	// data object (a keyedhash with a NULL scheme holds opaque user data).
	algKeyedHash uint16 = 0x0008
	// algAES is TPM_ALG_AES (0x0006): the symmetric algorithm a storage key
	// uses to protect its children.
	algAES uint16 = 0x0006
	// algCFB is TPM_ALG_CFB (0x0043): the block-cipher mode for that
	// child-protection symmetric cipher.
	algCFB uint16 = 0x0043
	// algSymCipher is TPM_ALG_SYMCIPHER, unused here but the family tag of a
	// symmetric block cipher object; the storage parent only references AES.
)

// storageObjectAttributes is the TPMA_OBJECT for the primary STORAGE key
// (the parent under which the sealed object is created and loaded):
//
//	fixedTPM | fixedParent | sensitiveDataOrigin |
//	userWithAuth | restricted | decrypt
//
//	= (1<<1)|(1<<4)|(1<<5)|(1<<6)|(1<<16)|(1<<17)
//	= 0x02 | 0x10 | 0x20 | 0x40 | 0x10000 | 0x20000
//	= 0x00030072
//
// A storage key is a RESTRICTED DECRYPT key: restricted+decrypt (NOT sign)
// is what makes it a parent able to wrap child sensitive areas. TCG "TPM
// 2.0 Part 2: Structures", clause "TPMA_OBJECT"; "Part 1: Architecture",
// "Protected Storage".
const (
	// objDecrypt (bit 17): the key may be used to decrypt (for a restricted
	// key, to unwrap/protect children).
	objDecrypt uint32 = 1 << 17

	storageObjectAttributes = objFixedTPM | objFixedParent | objSensitiveDataOrigin |
		objUserWithAuth | objRestricted | objDecrypt
)

// sealObjectAttributes is the TPMA_OBJECT for the SEALED keyedhash object.
// Crucially it sets fixedTPM | fixedParent but NOT userWithAuth: with
// userWithAuth clear, the object's userAuth cannot authorize use, so the
// ONLY way to satisfy the auth role is the authPolicy — i.e. a policy
// session that reproduces the sealed-to PCR state. TCG "TPM 2.0 Part 1:
// Architecture", "Enhanced Authorization"; "Part 2", "TPMA_OBJECT".
//
//	fixedTPM | fixedParent = (1<<1)|(1<<4) = 0x00000012
const sealObjectAttributes = objFixedTPM | objFixedParent

// PolicyPCRDigest computes, offline, the policyDigest that a TPM2_PolicyPCR
// over a SHA-256 policy session would accumulate for the given selection and
// PCR values. It is the value placed in the sealed object's authPolicy so
// the TPM will release the secret only to a session whose policyDigest
// matches.
//
// Per TCG "TPM 2.0 Part 1: Architecture", clause "TPM2_PolicyPCR" / "Policy
// Digest" (the policy update rule), with SHA-256 as the session's authHash:
//
//	policyDigestStart = 0x00 * 32                  (an all-zero digest)
//	pcrDigest         = SHA256( value_0 || value_1 || ... )  over the
//	                    selected PCR values in selection order
//	policyDigest      = SHA256( policyDigestStart
//	                            || TPM_CC_PolicyPCR (0x0000017F, big-endian)
//	                            || marshal(TPML_PCR_SELECTION pcrs)
//	                            || pcrDigest )
//
// The TPML_PCR_SELECTION marshaled into the hash is the SAME selection sent
// to TPM2_PolicyPCR. sel and pcrValues must be in corresponding order (the
// ascending order PCRRead returns). TCG "TPM 2.0 Part 1", "Policy PCR";
// "Part 3", "TPM2_PolicyPCR".
func PolicyPCRDigest(sel []PCRSelection, pcrValues [][]byte) []byte {
	// pcrDigest = H( concatenation of the selected PCR values ).
	ph := sha256.New()
	for _, v := range pcrValues {
		ph.Write(v)
	}
	pcrDigest := ph.Sum(nil)

	// policyDigest update over the zero start digest.
	dh := sha256.New()
	dh.Write(make([]byte, sha256.Size))    // policyDigestold = 0*32
	dh.Write(ccBytes(ccPolicyPCR))         // TPM_CC_PolicyPCR
	dh.Write(marshalPCRSelectionList(sel)) // TPML_PCR_SELECTION
	dh.Write(pcrDigest)                    // pcrDigest
	return dh.Sum(nil)
}

// ccBytes renders a TPM_CC as its 4-byte big-endian wire form, the way it is
// fed into a policyDigest update (TCG "Part 1", policy digest construction).
func ccBytes(cc common.TPM_CC) []byte {
	return common.PutU32(nil, uint32(cc))
}

// CreateStoragePrimary runs TPM2_CreatePrimary under TPM_RH_OWNER with empty
// password authorization, creating a primary ECC P-256 RESTRICTED DECRYPT
// (storage) key — the standard SRK-shaped parent under which a sealed object
// is created and loaded. It returns the transient parent handle.
//
// The TPMT_PUBLIC differs from the AK's in two ways that define a storage
// key (TCG "TPM 2.0 Part 2: Structures", "TPMT_PUBLIC", "TPMS_ECC_PARMS"):
//
//   - objectAttributes = restricted|decrypt (storageObjectAttributes), not
//     restricted|sign;
//   - the TPMS_ECC_PARMS.symmetric is a real TPMT_SYM_DEF_OBJECT
//     (AES-128-CFB), because a parent MUST carry a symmetric algorithm to
//     protect (encrypt) the sensitive areas of its children. A signing key
//     uses symmetric=NULL; a storage key cannot. (TCG "Part 1",
//     "Protected Storage".)
//
// scheme=NULL, curve=P256, kdf=NULL as for the AK.
//
// Wire: TagSessions, CC 0x00000131. TCG "Part 3", "TPM2_CreatePrimary".
func (tpm *TPM) CreateStoragePrimary() (handle uint32, err error) {
	body := common.PutU32(nil, uint32(common.RHOwner)) // primaryHandle

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = append(body, marshalEmptySensitiveCreate()...)
	body = append(body, marshalECCStoragePublic()...)
	body = common.PutU16(body, 0) // outsideInfo: empty TPM2B_DATA
	body = common.PutU32(body, 0) // creationPCR: TPML_PCR_SELECTION count 0

	rp, err := tpm.execute(common.TagSessions, common.CCCreatePrimary, body)
	if err != nil {
		return 0, err
	}
	// Response handle is the first u32; the rest (parameterSize, outPublic,
	// creation data, name) is not needed to use the parent.
	h, ok := common.GetU32(rp, 0)
	if !ok {
		return 0, common.ErrShortBuffer
	}
	return h, nil
}

// marshalECCStoragePublic renders the TPM2B_PUBLIC for the ECC P-256
// restricted-decrypt storage parent. The inner TPMT_PUBLIC is:
//
//	type             = TPM_ALG_ECC      (0x0023)
//	nameAlg          = TPM_ALG_SHA256   (0x000B)
//	objectAttributes = storageObjectAttributes (0x00030072)
//	authPolicy       = empty TPM2B_DIGEST (0x0000)
//	parameters       = TPMS_ECC_PARMS {
//	                     symmetric = TPMT_SYM_DEF_OBJECT {
//	                                   algorithm = TPM_ALG_AES (0x0006)
//	                                   keyBits   = 128 (0x0080)
//	                                   mode      = TPM_ALG_CFB (0x0043)
//	                                 }
//	                     scheme    = TPM_ALG_NULL (0x0010)
//	                     curveID   = TPM_ECC_NIST_P256 (0x0003)
//	                     kdf       = TPM_ALG_NULL (0x0010)
//	                   }
//	unique           = TPMS_ECC_POINT { x: empty, y: empty }
//
// TCG "Part 2", "TPMT_PUBLIC", "TPMS_ECC_PARMS", "TPMT_SYM_DEF_OBJECT".
func marshalECCStoragePublic() []byte {
	var p []byte
	p = common.PutU16(p, AlgECC)                  // type
	p = common.PutU16(p, algSHA256)               // nameAlg
	p = common.PutU32(p, storageObjectAttributes) // objectAttributes
	p = common.PutU16(p, 0)                       // authPolicy: empty TPM2B
	// TPMS_ECC_PARMS:
	//   symmetric = TPMT_SYM_DEF_OBJECT(AES, 128, CFB)
	p = common.PutU16(p, algAES)      // symmetric.algorithm = AES
	p = common.PutU16(p, 128)         // symmetric.keyBits.aes = 128
	p = common.PutU16(p, algCFB)      // symmetric.mode.aes = CFB
	p = common.PutU16(p, algNull)     // scheme = NULL
	p = common.PutU16(p, ECCNistP256) // curveID = NIST P-256
	p = common.PutU16(p, algNull)     // kdf = NULL
	// unique: empty ECC point.
	p = common.PutU16(p, 0) // x: empty TPM2B
	p = common.PutU16(p, 0) // y: empty TPM2B
	return common.MarshalTPM2B(p)
}

// marshalSealSensitiveCreate renders TPM2B_SENSITIVE_CREATE carrying the
// secret to be sealed: a TPMS_SENSITIVE_CREATE { userAuth: empty TPM2B,
// data: TPM2B(secret) }. The TPM stores `data` as the object's sensitive
// payload, which TPM2_Unseal later returns verbatim. TCG "Part 2",
// "TPMS_SENSITIVE_CREATE"; "Part 3", "TPM2_Create" (sealed data).
func marshalSealSensitiveCreate(secret []byte) []byte {
	inner := common.PutU16(nil, 0)                        // userAuth: empty TPM2B
	inner = append(inner, common.MarshalTPM2B(secret)...) // data: TPM2B(secret)
	return common.MarshalTPM2B(inner)
}

// marshalSealPublic renders the TPM2B_PUBLIC for the sealed keyedhash object
// bound to authPolicy. The inner TPMT_PUBLIC is:
//
//	type             = TPM_ALG_KEYEDHASH (0x0008)
//	nameAlg          = TPM_ALG_SHA256    (0x000B)
//	objectAttributes = sealObjectAttributes (0x00000012; fixedTPM|fixedParent,
//	                   NO userWithAuth -> policy-only authorization)
//	authPolicy       = TPM2B_DIGEST(policyDigest)
//	parameters       = TPMS_KEYEDHASH_PARMS { scheme = TPM_ALG_NULL (0x0010) }
//	unique           = empty TPM2B (keyedhash unique is a single TPM2B_DIGEST)
//
// A NULL keyedhash scheme means the object is a plain sealed-data blob (no
// HMAC/XOR derivation). TCG "Part 2", "TPMT_PUBLIC",
// "TPMS_KEYEDHASH_PARMS", "TPMU_PUBLIC_ID".
func marshalSealPublic(policyDigest []byte) []byte {
	var p []byte
	p = common.PutU16(p, algKeyedHash)                  // type
	p = common.PutU16(p, algSHA256)                     // nameAlg
	p = common.PutU32(p, sealObjectAttributes)          // objectAttributes
	p = append(p, common.MarshalTPM2B(policyDigest)...) // authPolicy
	p = common.PutU16(p, algNull)                       // scheme = NULL
	p = common.PutU16(p, 0)                             // unique: empty TPM2B_DIGEST
	return common.MarshalTPM2B(p)
}

// Create runs TPM2_Create under the parent (empty-auth password session),
// creating a sealed keyedhash object that holds `secret` and is bound to
// `policyDigest`. It returns the object's TPM2B_PRIVATE and TPM2B_PUBLIC,
// which Load later turns back into a transient handle.
//
// Body (TagSessions):
//
//	handle area: parentHandle (u32)
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_SENSITIVE_CREATE inSensitive (userAuth empty, the
//	             secret as data) || TPM2B_PUBLIC inPublic (keyedhash, sealed
//	             object, authPolicy=policyDigest) || TPM2B_DATA outsideInfo
//	             (empty) || TPML_PCR_SELECTION creationPCR (count 0)
//
// Wire: TagSessions, CC 0x00000153. TCG "Part 3", "TPM2_Create".
//
// Response (TagSessions): parameterSize (u32) || TPM2B_PRIVATE outPrivate ||
// TPM2B_PUBLIC outPublic || (creation data/hash/ticket, not parsed).
func (tpm *TPM) Create(parent uint32, secret, policyDigest []byte) (priv, pub []byte, err error) {
	body := common.PutU32(nil, parent) // parentHandle

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = append(body, marshalSealSensitiveCreate(secret)...)
	body = append(body, marshalSealPublic(policyDigest)...)
	body = common.PutU16(body, 0) // outsideInfo: empty TPM2B_DATA
	body = common.PutU32(body, 0) // creationPCR: count 0

	rp, err := tpm.execute(common.TagSessions, common.CCCreate, body)
	if err != nil {
		return nil, nil, err
	}
	// parameterSize (TagSessions response).
	if _, ok := common.GetU32(rp, 0); !ok {
		return nil, nil, common.ErrShortBuffer
	}
	rest := rp[4:]
	privVal, rest, err := common.UnmarshalTPM2B(rest)
	if err != nil {
		return nil, nil, err
	}
	pubVal, _, err := common.UnmarshalTPM2B(rest)
	if err != nil {
		return nil, nil, err
	}
	privCopy := make([]byte, len(privVal))
	copy(privCopy, privVal)
	pubCopy := make([]byte, len(pubVal))
	copy(pubCopy, pubVal)
	return privCopy, pubCopy, nil
}

// Load runs TPM2_Load under the parent (empty-auth password session),
// turning a sealed object's (priv, pub) pair back into a transient object
// handle usable by Unseal.
//
// Body (TagSessions):
//
//	handle area: parentHandle (u32)
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_PRIVATE inPrivate || TPM2B_PUBLIC inPublic
//
// Wire: TagSessions, CC 0x00000157. TCG "Part 3", "TPM2_Load".
//
// Response (TagSessions): objectHandle (u32) || parameterSize (u32) ||
// TPM2B_NAME name (not parsed).
func (tpm *TPM) Load(parent uint32, priv, pub []byte) (handle uint32, err error) {
	body := common.PutU32(nil, parent) // parentHandle

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = append(body, common.MarshalTPM2B(priv)...) // inPrivate
	body = append(body, common.MarshalTPM2B(pub)...)  // inPublic

	rp, err := tpm.execute(common.TagSessions, common.CCLoad, body)
	if err != nil {
		return 0, err
	}
	h, ok := common.GetU32(rp, 0)
	if !ok {
		return 0, common.ErrShortBuffer
	}
	return h, nil
}

// StartAuthSession runs TPM2_StartAuthSession to open a POLICY session
// (TPM_SE_POLICY) with SHA-256 as its authHash, unsalted and unbound
// (tpmKey = bind = TPM_RH_NULL), no parameter encryption (symmetric =
// TPM_ALG_NULL). nonceCaller is the caller's initial nonce (32 random bytes
// for SHA-256). It returns the session handle and the TPM's nonceTPM.
//
// Body (TagNoSessions — StartAuthSession itself carries no auth area):
//
//	handle area: tpmKey (u32) = TPM_RH_NULL, bind (u32) = TPM_RH_NULL
//	param  area: TPM2B_NONCE nonceCaller || TPM2B_ENCRYPTED_SECRET
//	             encryptedSalt (empty) || TPM_SE sessionType (u8) ||
//	             TPMT_SYM_DEF symmetric (algorithm = TPM_ALG_NULL) ||
//	             TPMI_ALG_HASH authHash (u16)
//
// A NULL TPMT_SYM_DEF is just the 2-byte algorithm id (no keyBits/mode).
//
// Wire: TagNoSessions, CC 0x00000176. TCG "Part 3", "TPM2_StartAuthSession";
// "Part 2", "TPMT_SYM_DEF", "TPM_SE".
//
// Response: sessionHandle (u32) || TPM2B_NONCE nonceTPM.
func (tpm *TPM) StartAuthSession(nonceCaller []byte) (sessionHandle uint32, nonceTPM []byte, err error) {
	body := common.PutU32(nil, rhNull)                       // tpmKey
	body = common.PutU32(body, rhNull)                       // bind
	body = append(body, common.MarshalTPM2B(nonceCaller)...) // nonceCaller
	body = common.PutU16(body, 0)                            // encryptedSalt: empty TPM2B
	body = common.PutU8(body, sePolicy)                      // sessionType = TPM_SE_POLICY
	body = common.PutU16(body, algNull)                      // symmetric = TPM_ALG_NULL
	body = common.PutU16(body, algSHA256)                    // authHash = SHA256

	rp, err := tpm.execute(common.TagNoSessions, ccStartAuthSession, body)
	if err != nil {
		return 0, nil, err
	}
	h, ok := common.GetU32(rp, 0)
	if !ok {
		return 0, nil, common.ErrShortBuffer
	}
	nt, _, err := common.UnmarshalTPM2B(rp[4:])
	if err != nil {
		return 0, nil, err
	}
	ntCopy := make([]byte, len(nt))
	copy(ntCopy, nt)
	return h, ntCopy, nil
}

// PolicyPCR runs TPM2_PolicyPCR in the given policy session, extending the
// session's policyDigest by the selected PCRs. pcrDigest is passed as an
// EMPTY TPM2B so the TPM uses the CURRENT PCR values to compute the digest
// it folds in — which means the session's resulting policyDigest equals
// PolicyPCRDigest(sel, currentPCRs). Unseal then succeeds iff that equals
// the sealed object's authPolicy.
//
// Body (TagNoSessions — PolicyPCR's session handle is in the HANDLE area,
// not an auth area):
//
//	handle area: policySession (u32)
//	param  area: TPM2B_DIGEST pcrDigest (empty) || TPML_PCR_SELECTION pcrs
//
// Wire: TagNoSessions, CC 0x0000017F. TCG "Part 3", "TPM2_PolicyPCR".
func (tpm *TPM) PolicyPCR(session uint32, sel []PCRSelection) error {
	body := common.PutU32(nil, session)                  // policySession
	body = common.PutU16(body, 0)                        // pcrDigest: empty TPM2B
	body = append(body, marshalPCRSelectionList(sel)...) // pcrs
	_, err := tpm.execute(common.TagNoSessions, ccPolicyPCR, body)
	return err
}

// PolicyGetDigest runs TPM2_PolicyGetDigest, reading back the current
// policyDigest accumulated in the session. It is a debugging aid: comparing
// it to PolicyPCRDigest confirms the offline construction matches what the
// TPM computed.
//
// Body (TagNoSessions): handle area policySession (u32).
// Response: TPM2B_DIGEST policyDigest.
//
// Wire: TagNoSessions, CC 0x00000189. TCG "Part 3", "TPM2_PolicyGetDigest".
func (tpm *TPM) PolicyGetDigest(session uint32) ([]byte, error) {
	body := common.PutU32(nil, session)
	rp, err := tpm.execute(common.TagNoSessions, ccPolicyGetDigest, body)
	if err != nil {
		return nil, err
	}
	d, _, err := common.UnmarshalTPM2B(rp)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(d))
	copy(out, d)
	return out, nil
}

// marshalPolicyAuth renders the TPMS_AUTH_COMMAND that carries a POLICY
// SESSION instead of the RS_PW password session:
//
//	[ sessionHandle:u32 = the policy session handle
//	| nonce:TPM2B       = 0x0000      (empty caller nonce)
//	| sessionAttributes:u8 = 0x01     (continueSession)
//	| hmac:TPM2B        = 0x0000      (empty: a satisfied policy session needs
//	                                   no HMAC for a no-auth-value object) ]
//
// The shape is identical to the password auth area; only the handle differs.
// This is what lets the TPM consult the session's policyDigest (built by
// PolicyPCR) to authorize Unseal. TCG "Part 1", "Enhanced Authorization";
// "Part 2", "TPMS_AUTH_COMMAND".
func marshalPolicyAuth(session uint32) []byte {
	out := common.PutU32(nil, session)       // sessionHandle (policy session)
	out = common.PutU16(out, 0)              // nonce: empty TPM2B
	out = common.PutU8(out, sessionContinue) // sessionAttributes
	out = common.PutU16(out, 0)              // hmac: empty TPM2B
	return out
}

// Unseal runs TPM2_Unseal on the loaded sealed object, authorizing it with
// the POLICY SESSION (which must already have been driven through PolicyPCR
// so its policyDigest matches the object's authPolicy). It returns the
// secret the object holds.
//
// Body (TagSessions):
//
//	handle area: itemHandle (u32) = the loaded sealed object
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND carrying the
//	             POLICY SESSION handle (marshalPolicyAuth)
//	(no parameters)
//
// Wire: TagSessions, CC 0x0000015E. TCG "Part 3", "TPM2_Unseal".
//
// Response (TagSessions): parameterSize (u32) || TPM2B_SENSITIVE_DATA
// outData (the secret).
func (tpm *TPM) Unseal(item, session uint32) ([]byte, error) {
	body := common.PutU32(nil, item) // itemHandle

	auth := marshalPolicyAuth(session)
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	rp, err := tpm.execute(common.TagSessions, ccUnseal, body)
	if err != nil {
		return nil, err
	}
	// parameterSize (TagSessions response).
	if _, ok := common.GetU32(rp, 0); !ok {
		return nil, common.ErrShortBuffer
	}
	d, _, err := common.UnmarshalTPM2B(rp[4:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(d))
	copy(out, d)
	return out, nil
}

// SealToPCR is the high-level seal helper: it computes the PCR-bound
// policyDigest from the given selection and PCR values, then runs TPM2_Create
// under parent to produce a sealed object holding secret. It returns the
// object's (priv, pub) blobs and the policyDigest it sealed to (so a caller
// can record/compare it). The PCR values must be the ones the object should
// later unseal against (typically the result of PCRRead just before sealing).
func (tpm *TPM) SealToPCR(parent uint32, secret []byte, sel []PCRSelection, pcrValues [][]byte) (priv, pub, policyDigest []byte, err error) {
	policyDigest = PolicyPCRDigest(sel, pcrValues)
	priv, pub, err = tpm.Create(parent, secret, policyDigest)
	if err != nil {
		return nil, nil, nil, err
	}
	return priv, pub, policyDigest, nil
}

// UnsealWithPCR is the high-level unseal helper: it loads the sealed object
// under parent, opens a fresh PCR policy session, runs PolicyPCR(sel) against
// the CURRENT PCR state, and unseals. It returns the secret on success, or
// the TPM's error (a policy failure if the current PCRs no longer match the
// sealed-to state). nonceCaller is the 32-byte caller nonce for the session.
func (tpm *TPM) UnsealWithPCR(parent uint32, priv, pub []byte, sel []PCRSelection, nonceCaller []byte) ([]byte, error) {
	item, err := tpm.Load(parent, priv, pub)
	if err != nil {
		return nil, err
	}
	session, _, err := tpm.StartAuthSession(nonceCaller)
	if err != nil {
		return nil, err
	}
	if err := tpm.PolicyPCR(session, sel); err != nil {
		return nil, err
	}
	return tpm.Unseal(item, session)
}
