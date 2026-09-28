// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import (
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"math/big"

	"github.com/go-tpm2/common"
)

// This file implements the CONTROL-PLANE side of TPM 2.0 Object Duplication /
// Import: wrapping an externally supplied secret so that a REMOTE node's TPM
// can TPM2_Import it under its storage key and then unseal it ONLY when its
// boot PCRs match a policy chosen at wrap time. The control plane has NO TPM;
// it works purely from the node's storage-key public point (x, y).
//
// The wrap is the OUTER WRAP of TCG "TPM 2.0 Part 1: Architecture", clause
// "Duplication" (and the shared "Protected Storage" outer-wrap primitives):
// it has exactly the same shape as MakeCredential's credential wrap, but
//
//   - it protects a full TPMT_SENSITIVE (the sealed object's sensitive area),
//     not a bare credential TPM2B; and
//   - the KDFe seed label is "DUPLICATE" (the duplication secret), NOT the
//     "IDENTITY" label of Credential Protection.
//
// With the node's storage key being ECC P-256 / SHA-256 nameAlg / AES-128-CFB
// child protection, and an ephemeral P-256 pair (de, Qe):
//
//	obf     := 32 random bytes                               // keyedhash obfuscation
//	unique  := SHA256(obf || secret)                         // public unique field
//	Z       := x-coord of de * SRK_pub                       // ECDH shared
//	seed    := KDFe(SHA256, Z, "DUPLICATE", Qe.x, SRK.x, 256)
//	symKey  := KDFa(SHA256, seed, "STORAGE",  objectName, nil, 128)
//	encSensitive := AES128-CFB(symKey, IV=0) over TPM2B(TPMT_SENSITIVE{obf,secret})
//	HMACkey := KDFa(SHA256, seed, "INTEGRITY", nil, nil, 256)
//	outerHMAC := HMAC-SHA256(HMACkey, encSensitive || objectName)
//	duplicate := TPM2B( TPM2B(outerHMAC) || encSensitive )
//	inSymSeed := TPM2B_ENCRYPTED_SECRET( TPMS_ECC_POINT{ Qe.x, Qe.y } )
//
// The TPMT_SENSITIVE carries a 32-byte obfuscation value (its seedValue) and
// the public's `unique` field is bound to SHA256(obf || secret): the TPM
// enforces this keyedhash public/sensitive binding (CryptValidateKeys) at the
// load that TPM2_Import performs internally, rejecting an empty seedValue or
// unique with TPM_RC_KEY_SIZE. Likewise the Import symmetricAlg is TPM_ALG_NULL
// (outer-wrap only; no inner wrapper) with an empty encryptionKey — a non-NULL
// symmetricAlg would make the TPM demand a key-sized encryptionKey and reject
// the empty one with TPM_RC_SIZE.
//
// The wrapped object is a KEYEDHASH sealed-data object whose authPolicy is the
// PolicyPCR digest over the chosen PCR selection/values, so the node can only
// release the secret in a policy session that reproduces those PCRs. Because
// the object is IMPORTED (not TPM-generated) its TPMA_OBJECT clears fixedTPM
// and fixedParent: an imported object is, by construction, NOT fixed to the
// TPM it lands on. TCG "Part 1", "Duplication" / "Protected Storage";
// "Part 2: Structures", "TPMA_OBJECT", "TPMT_SENSITIVE", "TPM2B_PRIVATE",
// "TPM2B_ENCRYPTED_SECRET"; "Part 3: Commands", "TPM2_Import".

// labelDuplicate is the KDFe seed label for an object-duplication outer wrap.
// TCG "TPM 2.0 Part 1: Architecture", "Duplication" (the secret-sharing seed
// for a duplicated object uses the "DUPLICATE" label, in contrast to the
// "IDENTITY" label of Credential Protection). The swtpm rejects any other
// label with an integrity/import failure, so this is load-bearing.
const labelDuplicate = "DUPLICATE"

// ccImport is TPM2_Import. TCG "TPM 2.0 Part 2: Structures", clause "TPM_CC
// (Command Codes)": TPM_CC_Import = 0x00000156. (common v0.1.0 predates this
// command, so the code is defined here.)
const ccImport common.TPM_CC = 0x00000156

// importSealAttributes is the TPMA_OBJECT for the IMPORTED sealed keyedhash
// object. It differs from seal.go's sealObjectAttributes (a TPM-CREATED sealed
// object) by clearing fixedTPM and fixedParent: an imported object cannot be
// fixed to a TPM (it originated off-TPM and was duplicated in), and Import
// REQUIRES both bits clear. As with the TPM-created sealed object, userWithAuth
// is also clear, so the ONLY way to authorize TPM2_Unseal is the authPolicy —
// i.e. a policy session reproducing the sealed-to PCR state.
//
//	objectAttributes = 0x00000000
//
// (No fixedTPM, no fixedParent, no userWithAuth, no sensitiveDataOrigin — the
// sensitive data came from outside.) TCG "TPM 2.0 Part 2: Structures",
// "TPMA_OBJECT"; "Part 1: Architecture", "Duplication" (importable objects
// have fixedTPM/fixedParent CLEAR); "Part 3", "TPM2_Import" (error if the
// object is fixedTPM or fixedParent).
const importSealAttributes uint32 = 0

// ECCPublic is an ECC P-256 public point (big-endian x, y) — the storage
// key's public the node exposes to the control plane. It is the same shape as
// EKPublic/AKPublic; a distinct name documents its role as the duplication
// PARENT public. TCG "TPM 2.0 Part 2: Structures", "TPMS_ECC_POINT".
type ECCPublic struct {
	// X is the big-endian x coordinate of the storage key's public point.
	X []byte
	// Y is the big-endian y coordinate of the storage key's public point.
	Y []byte
}

// WrapToPCRResult is the output of WrapToPCR: the three blobs the control
// plane ships to the node for TPM2_Import.
type WrapToPCRResult struct {
	// ObjectPublic is the marshaled TPMT_PUBLIC (the CONTENTS of TPM2B_PUBLIC,
	// without the 2-byte size prefix) of the sealed keyedhash object. It is
	// passed to Import (which wraps it in a TPM2B_PUBLIC) and later to Load.
	ObjectPublic []byte
	// Duplicate is the TPM2B_PRIVATE-shaped duplication blob:
	// TPM2B( TPM2B(outerHMAC) || encSensitive ).
	Duplicate []byte
	// InSymSeed is the TPM2B_ENCRYPTED_SECRET carrying the ephemeral public
	// point (TPMS_ECC_POINT) the node's TPM uses to recover the outer-wrap
	// seed by ECDH against its storage key.
	InSymSeed []byte
}

// WrapToPCR performs the OFFLINE, no-TPM control-plane side of object
// duplication: it builds a KEYEDHASH sealed-data object holding secret and
// bound to authPolicy = PolicyPCRDigest(sel, pcrValues), then OUTER-WRAPS its
// sensitive area for the node's ECC P-256 storage key srkPub. The returned
// (ObjectPublic, Duplicate, InSymSeed) are exactly the inputs to TPM2_Import.
//
// sel/pcrValues are the PCR selection and the values the object must unseal
// against (the node's boot state); they are folded into the object's
// authPolicy via the same PolicyPCRDigest construction the seal/unseal path
// uses, so a node can Import + Load the object but only Unseal it when its
// live PCRs reproduce pcrValues.
//
// The ephemeral key is drawn from crypto/rand; rng may override the source
// for deterministic testing (pass nil for crypto/rand).
//
// TCG "TPM 2.0 Part 1: Architecture", "Duplication" / "Protected Storage"
// (the outer wrap: KDFe seed "DUPLICATE", KDFa "STORAGE"/"INTEGRITY", the
// HMAC over encSensitive||Name); "Part 2", "TPMT_SENSITIVE", "TPMT_PUBLIC".
func WrapToPCR(srkPub ECCPublic, secret []byte, sel []PCRSelection, pcrValues [][]byte, rng io.Reader) (WrapToPCRResult, error) {
	if rng == nil {
		rng = rand.Reader
	}
	curve := elliptic.P256()

	// Validate the storage key's public point lies on P-256.
	srkX := new(big.Int).SetBytes(srkPub.X)
	srkY := new(big.Int).SetBytes(srkPub.Y)
	if !curve.IsOnCurve(srkX, srkY) {
		return WrapToPCRResult{}, ErrSRKPointNotOnCurve
	}

	// 1. The keyedhash OBFUSCATION value (the sensitive's seedValue). For a
	//    keyedhash object with a (non-NULL) nameAlg the TPM REQUIRES, at load,
	//    that seedValue be exactly nameAlg-digest sized and that the public
	//    `unique` equal H_nameAlg(seedValue || data) — CryptValidateKeys ->
	//    CryptComputeSymmetricUnique (non-restricted -> plain hash). A live
	//    swtpm enforces this during Import's internal ObjectLoad: an EMPTY
	//    seedValue and EMPTY unique are rejected with TPM_RC_KEY_SIZE (rc
	//    0x3C7, param 3 = duplicate) on the seedValue-size check. So draw a
	//    32-byte obfuscation value and bind unique to it. TCG "TPM 2.0 Part 1:
	//    Architecture", "Protected Storage" (keyedhash obfuscation value);
	//    "Part 2", "TPMT_SENSITIVE" (seedValue), "Object Generation".
	obfuscate := make([]byte, sha256.Size)
	if _, err := io.ReadFull(rng, obfuscate); err != nil {
		return WrapToPCRResult{}, err
	}
	// unique = H_nameAlg( seedValue || data ) over the RAW buffers (no TPM2B
	// size prefixes), the value CryptComputeSymmetricUnique recomputes and
	// CryptValidateKeys compares against the public `unique`. TCG "Part 4:
	// Supporting Routines", CryptComputeSymmetricUnique.
	uh := sha256.New()
	uh.Write(obfuscate)
	uh.Write(secret)
	unique := uh.Sum(nil)

	// 2. The sealed object's public area: KEYEDHASH, authPolicy = PolicyPCR
	//    digest over the chosen PCRs, importable attributes (fixedTPM/
	//    fixedParent clear), unique = H(seedValue||data). The object's Name is
	//    H_nameAlg(TPMT_PUBLIC), which is committed into the wrap (KDFa context
	//    + outer HMAC).
	policyDigest := PolicyPCRDigest(sel, pcrValues)
	objectPublic := marshalImportSealPublic(policyDigest, unique)
	// The Name is nameAlg || H_nameAlg(TPMT_PUBLIC). marshalImportSealPublic
	// hard-codes the SHA-256 nameAlg, so the Name is computed directly here
	// (rather than via the fallible ObjectName, whose only error path —
	// unsupported nameAlg — is unreachable for this fixed template). TCG
	// "Part 1: Architecture", "Names".
	objectName := importObjectName(objectPublic)

	// 3. The sensitive area to protect: a TPMT_SENSITIVE keyedhash carrying
	//    the obfuscation value (seedValue) and the secret as its sensitive
	//    payload (the value Unseal returns).
	sensitive := marshalSealSensitive(obfuscate, secret)

	// 4. Ephemeral P-256 key pair (the duplication "secret"): de private,
	//    Qe = de*G public.
	eph, err := generateEphemeralKey(curve, rng)
	if err != nil {
		return WrapToPCRResult{}, err
	}

	// 5. ECDH: Z = x-coord of de * SRK_pub, fixed-width 32-byte big-endian.
	zx, _ := curve.ScalarMult(srkX, srkY, eph.D.Bytes())
	z := leftPad(zx.Bytes(), p256FieldBytes)

	qeX := leftPad(eph.X.Bytes(), p256FieldBytes)
	qeY := leftPad(eph.Y.Bytes(), p256FieldBytes)
	srkXfix := leftPad(srkPub.X, p256FieldBytes)

	// 6. seed = KDFe(Z, "DUPLICATE", partyU=Qe.x, partyV=SRK.x, 256).
	seed := KDFe(z, labelDuplicate, qeX, srkXfix, 256)

	// 7. symKey = KDFa(seed, "STORAGE", context=objectName, nil, 128).
	symKey := KDFa(seed, labelStorage, objectName, nil, 128)

	// 8. encSensitive = AES-128-CFB(symKey, IV=0) over TPM2B(TPMT_SENSITIVE).
	plain := common.MarshalTPM2B(sensitive)
	encSensitive := aesCFBZeroIV(symKey, plain)

	// 9. HMACkey = KDFa(seed, "INTEGRITY", nil, nil, 256).
	hmacKey := KDFa(seed, labelIntegrity, nil, nil, 256)

	// 10. outerHMAC = HMAC-SHA256(HMACkey, encSensitive || objectName).
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(encSensitive)
	mac.Write(objectName)
	outerHMAC := mac.Sum(nil)

	// 11. duplicate = TPM2B( TPM2B(outerHMAC) || encSensitive ).
	dup := common.MarshalTPM2B(outerHMAC)
	dup = append(dup, encSensitive...)
	duplicate := common.MarshalTPM2B(dup)

	// 12. inSymSeed = TPM2B_ENCRYPTED_SECRET( TPMS_ECC_POINT{ Qe.x, Qe.y } ).
	point := common.MarshalTPM2B(qeX)
	point = append(point, common.MarshalTPM2B(qeY)...)
	inSymSeed := common.MarshalTPM2B(point)

	return WrapToPCRResult{
		ObjectPublic: objectPublic,
		Duplicate:    duplicate,
		InSymSeed:    inSymSeed,
	}, nil
}

// marshalImportSealPublic renders the TPMT_PUBLIC (the CONTENTS, no TPM2B
// prefix) for the IMPORTED sealed keyedhash object bound to authPolicy. It
// mirrors seal.go's marshalSealPublic but (a) returns the bare TPMT_PUBLIC
// (the wrap commits to the Name = H(TPMT_PUBLIC), and Import takes a
// TPM2B_PUBLIC built from these bytes), and (b) uses importSealAttributes
// (fixedTPM/fixedParent CLEAR) so the object is importable:
//
//	type             = TPM_ALG_KEYEDHASH (0x0008)
//	nameAlg          = TPM_ALG_SHA256    (0x000B)
//	objectAttributes = importSealAttributes (0x00000000)
//	authPolicy       = TPM2B_DIGEST(policyDigest)
//	parameters       = TPMS_KEYEDHASH_PARMS { scheme = TPM_ALG_NULL (0x0010) }
//	unique           = TPM2B_DIGEST(unique)  // = H_nameAlg(seedValue || data)
//
// The `unique` field is NOT empty: for a keyedhash object with a nameAlg the
// TPM's load-time CryptValidateKeys requires unique == H_nameAlg(seedValue ||
// sensitiveData) and a digest-sized seedValue; an empty unique/seedValue is
// rejected with TPM_RC_KEY_SIZE during Import. TCG "Part 2", "TPMT_PUBLIC",
// "TPMS_KEYEDHASH_PARMS", "TPMU_PUBLIC_ID"; "Part 4", CryptComputeSymmetricUnique.
func marshalImportSealPublic(policyDigest, unique []byte) []byte {
	var p []byte
	p = common.PutU16(p, algKeyedHash)                  // type
	p = common.PutU16(p, algSHA256)                     // nameAlg
	p = common.PutU32(p, importSealAttributes)          // objectAttributes
	p = append(p, common.MarshalTPM2B(policyDigest)...) // authPolicy
	p = common.PutU16(p, algNull)                       // scheme = NULL
	p = append(p, common.MarshalTPM2B(unique)...)       // unique: TPM2B_DIGEST
	return p
}

// importObjectName computes the Name of the imported sealed object directly
// from its TPMT_PUBLIC: Name = nameAlg(u16) || SHA256(TPMT_PUBLIC). The
// template's nameAlg is fixed to SHA-256, so this never has to branch on the
// hash algorithm (unlike the general ObjectName). TCG "TPM 2.0 Part 1:
// Architecture", "Names".
func importObjectName(publicArea []byte) []byte {
	digest := sha256.Sum256(publicArea)
	name := common.PutU16(nil, algSHA256)
	return append(name, digest[:]...)
}

// marshalSealSensitive renders the TPMT_SENSITIVE (the CONTENTS, no TPM2B
// prefix) of an imported sealed keyedhash object carrying secret, with the
// keyedhash obfuscation value seedValue:
//
//	sensitiveType = TPM_ALG_KEYEDHASH (0x0008)     // matches the public type
//	authValue     = empty TPM2B_DIGEST  (0x0000)
//	seedValue     = TPM2B_DIGEST(seedValue)        // obfuscation value, 32 bytes
//	sensitive     = TPM2B_SENSITIVE_DATA(secret)   // the sealed payload
//
// For a keyedhash data object the sensitive union is TPM2B_SENSITIVE_DATA,
// which TPM2_Unseal returns verbatim. The seedValue is the keyedhash
// obfuscation value; for an object with a nameAlg the TPM requires it to be
// nameAlg-digest sized and to bind the public `unique` = H(seedValue||data),
// so it must NOT be empty (an empty seedValue triggers TPM_RC_KEY_SIZE at
// load). TCG "TPM 2.0 Part 2: Structures", "TPMT_SENSITIVE",
// "TPMU_SENSITIVE_COMPOSITE"; "Part 1", "Protected Storage" (obfuscation value).
func marshalSealSensitive(seedValue, secret []byte) []byte {
	var s []byte
	s = common.PutU16(s, algKeyedHash)               // sensitiveType
	s = common.PutU16(s, 0)                          // authValue: empty TPM2B
	s = append(s, common.MarshalTPM2B(seedValue)...) // seedValue: TPM2B_DIGEST
	s = append(s, common.MarshalTPM2B(secret)...)    // sensitive: TPM2B data
	return s
}

// Import runs TPM2_Import under parent (empty-auth RS_PW password session),
// importing a duplicated object into the parent's hierarchy. It returns the
// object's outPrivate (TPM2B_PRIVATE), which Load turns into a transient
// handle.
//
// objectPublic is the bare TPMT_PUBLIC (as produced by WrapToPCR); Import
// wraps it in a TPM2B_PUBLIC. duplicate and inSymSeed are WrapToPCR's outputs,
// each already a complete TPM2B (TPM2B_PRIVATE and TPM2B_ENCRYPTED_SECRET
// respectively) and emitted on the wire verbatim.
//
// Body (TagSessions):
//
//	handle area: parentHandle (u32)
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_DATA encryptionKey (empty) ||
//	             TPM2B_PUBLIC objectPublic ||
//	             TPM2B_PRIVATE duplicate ||
//	             TPM2B_ENCRYPTED_SECRET inSymSeed ||
//	             TPMT_SYM_DEF_OBJECT symmetricAlg = TPM_ALG_NULL (no inner wrap)
//
// encryptionKey is empty AND symmetricAlg is TPM_ALG_NULL because WrapToPCR
// produces an OUTER-WRAP-ONLY duplication: there is no INNER symmetric wrap of
// the sensitive area. The Import symmetricAlg argument describes the optional
// INNER wrapper on `duplicate`, NOT the parent's child-protection cipher; with
// no inner wrap it MUST be NULL and encryptionKey MUST be empty (per TCG and
// the go-tpm reference: "encryptionKey and sym non-nil iff an inner wrapper is
// used"). Passing the parent's AES-128-CFB here makes the TPM expect a
// non-empty encryptionKey of the AES key size and reject the empty one with
// TPM_RC_SIZE on parameter 1 (rc 0x1D5) — the bug a real swtpm caught.
// Wire: TagSessions, CC 0x00000156. TCG "TPM 2.0 Part 3: Commands",
// "TPM2_Import"; "Part 2", "TPMT_SYM_DEF_OBJECT".
//
// Response (TagSessions): parameterSize (u32) || TPM2B_PRIVATE outPrivate.
func (tpm *TPM) Import(parent uint32, objectPublic, duplicate, inSymSeed []byte) (outPrivate []byte, err error) {
	body := common.PutU32(nil, parent) // parentHandle

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = common.PutU16(body, 0)                             // encryptionKey: empty TPM2B_DATA
	body = append(body, common.MarshalTPM2B(objectPublic)...) // objectPublic: TPM2B_PUBLIC
	body = append(body, duplicate...)                         // duplicate: TPM2B_PRIVATE
	body = append(body, inSymSeed...)                         // inSymSeed: TPM2B_ENCRYPTED_SECRET
	body = append(body, marshalSymDefObjectNull()...)         // symmetricAlg = NULL (no inner wrap)

	rp, err := tpm.execute(common.TagSessions, ccImport, body)
	if err != nil {
		return nil, err
	}
	// parameterSize (TagSessions response).
	if _, ok := common.GetU32(rp, 0); !ok {
		return nil, common.ErrShortBuffer
	}
	priv, _, err := common.UnmarshalTPM2B(rp[4:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(priv))
	copy(out, priv)
	return out, nil
}

// marshalSymDefObjectNull renders the TPMT_SYM_DEF_OBJECT for TPM2_Import's
// symmetricAlg argument as TPM_ALG_NULL — a single 2-byte algorithm id with no
// keyBits/mode tail. This argument describes the optional INNER symmetric
// wrapper applied to `duplicate`; WrapToPCR uses OUTER-WRAP ONLY (the sensitive
// area is protected by the seed-derived KDFa "STORAGE" key, carried via
// inSymSeed), so there is no inner wrapper and the scheme is NULL.
//
// It must NOT be the parent storage key's AES-128-CFB child-protection cipher:
// that would tell the TPM an inner wrapper is present and make it require a
// non-empty encryptionKey of the AES key size, rejecting our empty
// encryptionKey with TPM_RC_SIZE on parameter 1 (rc 0x1D5). TCG "TPM 2.0
// Part 3: Commands", "TPM2_Import" (symmetricAlg / encryptionKey describe the
// inner wrapper); "Part 2", "TPMT_SYM_DEF_OBJECT" (a NULL algorithm has no
// keyBits/mode).
func marshalSymDefObjectNull() []byte {
	return common.PutU16(nil, algNull) // algorithm = TPM_ALG_NULL
}

// ImportAndUnseal is the high-level node-side recover path: it TPM2_Imports
// the duplicated object under parent, TPM2_Loads the resulting (outPrivate,
// objectPublic) into a transient handle, opens a fresh PCR policy session,
// runs PolicyPCR(sel) against the node's CURRENT PCR state, and Unseals. It
// returns the recovered secret on success, or the TPM's error — a policy
// failure (TPM_RC_POLICY_FAIL) if the live PCRs no longer match the values
// the control plane wrapped to.
//
// objectPublic, duplicate, inSymSeed are exactly WrapToPCR's outputs; sel is
// the same PCR selection used at wrap time. nonceCaller is the 32-byte caller
// nonce for the policy session. This ties TPM2_Import -> TPM2_Load ->
// TPM2_PolicyPCR -> TPM2_Unseal into one call.
func (tpm *TPM) ImportAndUnseal(parent uint32, objectPublic, duplicate, inSymSeed []byte, sel []PCRSelection, nonceCaller []byte) ([]byte, error) {
	outPrivate, err := tpm.Import(parent, objectPublic, duplicate, inSymSeed)
	if err != nil {
		return nil, err
	}
	item, err := tpm.Load(parent, outPrivate, objectPublic)
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

// ErrSRKPointNotOnCurve is returned by WrapToPCR when the supplied storage-key
// public point does not lie on NIST P-256 (a malformed or wrong SRK public).
const ErrSRKPointNotOnCurve = common.Error("tpm2: SRK public point is not on the P-256 curve")
