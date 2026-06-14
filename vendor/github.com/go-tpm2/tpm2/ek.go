// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import "github.com/go-tpm2/common"

// This file creates the Endorsement Key (EK): the primary, restricted-decrypt
// ECC P-256 key under the ENDORSEMENT hierarchy, built from the well-known
// "low-range" L-2 template of the TCG "EK Credential Profile for TPM Family
// 2.0" (the ECC NIST P256 EK template). The EK is deterministic from the
// endorsement primary seed, so two CreatePrimary calls with the same template
// return the same public point. TCG "EK Credential Profile", "Template L-2:
// ECC NIST P256"; "TPM 2.0 Part 1: Architecture", "Endorsement Hierarchy".

// RHEndorsement is TPM_RH_ENDORSEMENT, the endorsement hierarchy permanent
// handle under which the EK primary is created. TCG "TPM 2.0 Part 2:
// Structures", clause "TPM_RH (Permanent Handles)".
const RHEndorsement uint32 = 0x4000000B

// EK object attribute bits not already named for the AK/storage keys. TCG
// "TPM 2.0 Part 2: Structures", clause "TPMA_OBJECT".
const (
	// objAdminWithPolicy (bit 7): operations in the ADMIN role on this object
	// require policy authorization (a satisfied authPolicy session), not the
	// userAuth. The EK uses adminWithPolicy (and NOT userWithAuth) so its use
	// is gated by the standard EK policy. TCG "Part 2", "TPMA_OBJECT".
	objAdminWithPolicy uint32 = 1 << 7

	// ekObjectAttributes is the TPMA_OBJECT of the EK Credential Profile L-2
	// template:
	//
	//	fixedTPM | fixedParent | sensitiveDataOrigin |
	//	adminWithPolicy | restricted | decrypt
	//
	//	= (1<<1)|(1<<4)|(1<<5)|(1<<7)|(1<<16)|(1<<17)
	//	= 0x02 | 0x10 | 0x20 | 0x80 | 0x10000 | 0x20000
	//	= 0x000300B2
	//
	// (No userWithAuth, no sign: the EK is a restricted decrypt key whose use
	// is authorized only via the well-known EK policy.) TCG "EK Credential
	// Profile", "Template L-2".
	ekObjectAttributes = objFixedTPM | objFixedParent | objSensitiveDataOrigin |
		objAdminWithPolicy | objRestricted | objDecrypt
)

// ekAuthPolicy is the well-known SHA-256 authorization policy of the EK
// Credential Profile templates: the policyDigest of
// TPM2_PolicySecret(TPM_RH_ENDORSEMENT). Every standard EK carries exactly
// this authPolicy in its template, so it is hard-coded here. TCG "EK
// Credential Profile for TPM Family 2.0", Appendix B (the SHA-256
// authPolicy): 837197674484b3f81a90cc8d46a5d724fd52d76e06520b64f2a1da1b331469aa.
var ekAuthPolicy = []byte{
	0x83, 0x71, 0x97, 0x67, 0x44, 0x84, 0xb3, 0xf8,
	0x1a, 0x90, 0xcc, 0x8d, 0x46, 0xa5, 0xd7, 0x24,
	0xfd, 0x52, 0xd7, 0x6e, 0x06, 0x52, 0x0b, 0x64,
	0xf2, 0xa1, 0xda, 0x1b, 0x33, 0x14, 0x69, 0xaa,
}

// EKPublic is the parsed public point of the ECC Endorsement Key: the affine
// (x, y) coordinates on NIST P-256, big-endian, from the TPMS_ECC_POINT in the
// key's TPMT_PUBLIC.unique. TCG "TPM 2.0 Part 2: Structures", clauses
// "TPMS_ECC_POINT" and "TPMT_PUBLIC".
type EKPublic struct {
	// X is the big-endian x coordinate of the public point.
	X []byte
	// Y is the big-endian y coordinate of the public point.
	Y []byte
}

// marshalEKPublic renders the TPM2B_PUBLIC for the EK Credential Profile L-2
// (ECC NIST P256) template. The inner TPMT_PUBLIC is:
//
//	type             = TPM_ALG_ECC      (0x0023)
//	nameAlg          = TPM_ALG_SHA256   (0x000B)
//	objectAttributes = ekObjectAttributes (0x000300B2)
//	authPolicy       = TPM2B_DIGEST(ekAuthPolicy)  (the well-known EK policy)
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
// The EK is a restricted DECRYPT (storage-shaped) key, so its symmetric is a
// real AES-128-CFB block cipher (not NULL); scheme and kdf are NULL. The
// authPolicy is the fixed EK policy, NOT empty — this is the defining
// difference from a generic storage primary. TCG "EK Credential Profile",
// "Template L-2"; "TPM 2.0 Part 2", "TPMT_PUBLIC", "TPMS_ECC_PARMS",
// "TPMT_SYM_DEF_OBJECT".
func marshalEKPublic() []byte {
	var p []byte
	p = common.PutU16(p, AlgECC)                        // type
	p = common.PutU16(p, algSHA256)                     // nameAlg
	p = common.PutU32(p, ekObjectAttributes)            // objectAttributes
	p = append(p, common.MarshalTPM2B(ekAuthPolicy)...) // authPolicy (EK policy)
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

// CreateEK runs TPM2_CreatePrimary under TPM_RH_ENDORSEMENT (empty-auth
// password session) with the EK Credential Profile L-2 ECC P-256 template,
// creating the Endorsement Key. It returns the transient EK handle and the
// EK's public point.
//
// Body (TagSessions):
//
//	handle area: primaryHandle (u32) = TPM_RH_ENDORSEMENT
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_SENSITIVE_CREATE (empty) || TPM2B_PUBLIC (EK template) ||
//	             TPM2B_DATA outsideInfo (empty) || TPML_PCR_SELECTION (count 0)
//
// Wire: TagSessions, CC 0x00000131. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_CreatePrimary"; "EK Credential Profile", "Template L-2".
//
// Response: objectHandle (u32) || parameterSize (u32) || TPM2B_PUBLIC outPublic
// (from which the EK point is extracted) || trailing creation fields.
func (tpm *TPM) CreateEK() (ekHandle uint32, pub EKPublic, err error) {
	body := common.PutU32(nil, RHEndorsement) // primaryHandle = endorsement

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = append(body, marshalEmptySensitiveCreate()...)
	body = append(body, marshalEKPublic()...)
	body = common.PutU16(body, 0) // outsideInfo: empty TPM2B_DATA
	body = common.PutU32(body, 0) // creationPCR: TPML_PCR_SELECTION count 0

	rp, err := tpm.execute(common.TagSessions, common.CCCreatePrimary, body)
	if err != nil {
		return 0, EKPublic{}, err
	}
	handle, ak, err := parseCreatePrimaryResponse(rp)
	if err != nil {
		return 0, EKPublic{}, err
	}
	return handle, EKPublic{X: ak.X, Y: ak.Y}, nil
}
