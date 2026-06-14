// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import "github.com/go-tpm2/common"

// AKPublic is the parsed public part of an ECC Attestation Key: the affine
// coordinates of the public point on NIST P-256, big-endian. X and Y are the
// raw TPM2B contents of the TPMS_ECC_POINT in the key's TPMT_PUBLIC.unique
// field. TCG "TPM 2.0 Part 2: Structures", clauses "TPMS_ECC_POINT" and
// "TPMT_PUBLIC".
type AKPublic struct {
	// X is the big-endian x coordinate of the public point.
	X []byte
	// Y is the big-endian y coordinate of the public point.
	Y []byte
}

// CreatePrimary runs TPM2_CreatePrimary under TPM_RH_OWNER with empty
// password authorization, creating a primary ECC P-256 restricted signing
// Attestation Key. It returns the transient object handle the TPM assigned to
// the new key and the key's public point.
//
// This command carries a session (authorization) area, so the body has three
// regions (TCG "TPM 2.0 Part 1: Architecture", "Command Authorization Area
// Structure"):
//
//	handle area: primaryHandle (u32) = TPM_RH_OWNER
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_SENSITIVE_CREATE || TPM2B_PUBLIC ||
//	             TPM2B_DATA outsideInfo || TPML_PCR_SELECTION creationPCR
//
// The TPM2B_SENSITIVE_CREATE wraps a TPMS_SENSITIVE_CREATE with empty
// userAuth and empty data (the TPM originates the key material), so its inner
// bytes are two empty TPM2B (0x0000 0x0000), size-prefixed to 0x0004.
//
// The TPM2B_PUBLIC wraps a TPMT_PUBLIC describing the ECC restricted signing
// key (see marshalECCAKPublic). outsideInfo is an empty TPM2B_DATA, and
// creationPCR is an empty TPML_PCR_SELECTION (count 0).
//
// Wire: TagSessions, CC 0x00000131. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_CreatePrimary".
//
// Response (TCG "Part 3", "TPM2_CreatePrimary" response):
//
//	handle area: objectHandle (u32)
//	(no auth area parsed here: it is present but trailing)
//	param  area: parameterSize (u32, present because TagSessions) ||
//	             TPM2B_PUBLIC outPublic || TPM2B_CREATION_DATA creationData ||
//	             TPM2B_DIGEST creationHash || TPMT_TK_CREATION creationTicket ||
//	             TPM2B_NAME name
//
// We parse outPublic to extract the AK's public point and consume (but do not
// further decode) the remaining creation fields.
func (tpm *TPM) CreatePrimary() (akHandle uint32, pub AKPublic, err error) {
	// Handle area: the owner hierarchy.
	body := common.PutU32(nil, uint32(common.RHOwner))

	// Auth area: empty-auth password session, length-prefixed.
	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	// Parameter area.
	body = append(body, marshalEmptySensitiveCreate()...)
	body = append(body, marshalECCAKPublic()...)
	body = common.PutU16(body, 0) // outsideInfo: empty TPM2B_DATA
	body = common.PutU32(body, 0) // creationPCR: TPML_PCR_SELECTION count 0

	rp, err := tpm.execute(common.TagSessions, common.CCCreatePrimary, body)
	if err != nil {
		return 0, AKPublic{}, err
	}
	return parseCreatePrimaryResponse(rp)
}

// CreatePrimaryPublic is CreatePrimary but also returns the AK's TPMT_PUBLIC
// bytes (the CONTENTS of the response TPM2B_PUBLIC, without the 2-byte size
// prefix). Those bytes are exactly what the TPM loaded, so ObjectName over
// them yields the AK's Name — the value MakeCredential must commit to. The
// credential-activation flow needs the Name, which cannot be reconstructed
// reliably from the template alone, so it is taken from the TPM's own
// outPublic here. TCG "Part 3", "TPM2_CreatePrimary" (outPublic); "Part 1",
// "Names".
func (tpm *TPM) CreatePrimaryPublic() (akHandle uint32, outPublic []byte, err error) {
	body := common.PutU32(nil, uint32(common.RHOwner))

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = append(body, marshalEmptySensitiveCreate()...)
	body = append(body, marshalECCAKPublic()...)
	body = common.PutU16(body, 0) // outsideInfo: empty TPM2B_DATA
	body = common.PutU32(body, 0) // creationPCR: TPML_PCR_SELECTION count 0

	rp, err := tpm.execute(common.TagSessions, common.CCCreatePrimary, body)
	if err != nil {
		return 0, nil, err
	}
	handle, ok := common.GetU32(rp, 0)
	if !ok {
		return 0, nil, common.ErrShortBuffer
	}
	// parameterSize (TagSessions): skip 4 bytes.
	if _, ok := common.GetU32(rp, 4); !ok {
		return 0, nil, common.ErrShortBuffer
	}
	pubBytes, _, err := common.UnmarshalTPM2B(rp[8:]) // outPublic TPM2B_PUBLIC
	if err != nil {
		return 0, nil, err
	}
	out := make([]byte, len(pubBytes))
	copy(out, pubBytes)
	return handle, out, nil
}

// marshalEmptySensitiveCreate renders TPM2B_SENSITIVE_CREATE for a
// TPM-originated key: the inner TPMS_SENSITIVE_CREATE is { userAuth: empty
// TPM2B, data: empty TPM2B } = 0x0000 0x0000 (4 bytes), wrapped by the
// 2-byte size prefix -> 0x0004 0x0000 0x0000. TCG "TPM 2.0 Part 2:
// Structures", clauses "TPMS_SENSITIVE_CREATE" and "TPM2B_SENSITIVE_CREATE".
func marshalEmptySensitiveCreate() []byte {
	inner := common.PutU16(nil, 0)  // userAuth: empty TPM2B
	inner = common.PutU16(inner, 0) // data: empty TPM2B
	return common.MarshalTPM2B(inner)
}

// marshalECCAKPublic renders the TPM2B_PUBLIC for an ECC P-256 restricted
// signing Attestation Key. The inner TPMT_PUBLIC is:
//
//	type            = TPM_ALG_ECC      (0x0023)
//	nameAlg         = TPM_ALG_SHA256   (0x000B)
//	objectAttributes= akObjectAttributes (0x00050072)
//	authPolicy      = empty TPM2B_DIGEST (0x0000)
//	parameters      = TPMS_ECC_PARMS {
//	                    symmetric = TPM_ALG_NULL (0x0010)
//	                    scheme    = TPMT_SIG_SCHEME {
//	                                  scheme = TPM_ALG_ECDSA (0x0018)
//	                                  details.hashAlg = TPM_ALG_SHA256 (0x000B)
//	                                }
//	                    curveID   = TPM_ECC_NIST_P256 (0x0003)
//	                    kdf       = TPM_ALG_NULL (0x0010)
//	                  }
//	unique          = TPMS_ECC_POINT { x: empty TPM2B, y: empty TPM2B }
//
// The whole TPMT_PUBLIC is then wrapped in a TPM2B_PUBLIC size prefix. TCG
// "TPM 2.0 Part 2: Structures", clauses "TPMT_PUBLIC", "TPMS_ECC_PARMS",
// "TPMT_ECC_SCHEME"/"TPMT_SIG_SCHEME", "TPMS_ECC_POINT".
func marshalECCAKPublic() []byte {
	var p []byte
	p = common.PutU16(p, AlgECC)             // type
	p = common.PutU16(p, algSHA256)          // nameAlg
	p = common.PutU32(p, akObjectAttributes) // objectAttributes
	p = common.PutU16(p, 0)                  // authPolicy: empty TPM2B
	// TPMS_ECC_PARMS:
	p = common.PutU16(p, algNull)     // symmetric = NULL
	p = common.PutU16(p, AlgECDSA)    // scheme = ECDSA
	p = common.PutU16(p, algSHA256)   // scheme.details.hashAlg
	p = common.PutU16(p, ECCNistP256) // curveID = NIST P-256
	p = common.PutU16(p, algNull)     // kdf = NULL
	// TPMS_ECC_POINT unique: empty x, empty y.
	p = common.PutU16(p, 0) // x: empty TPM2B
	p = common.PutU16(p, 0) // y: empty TPM2B
	return common.MarshalTPM2B(p)
}

// parseCreatePrimaryResponse decodes the TPM2_CreatePrimary response: the
// objectHandle, then (because the command was TagSessions) a parameterSize
// u32, then TPM2B_PUBLIC outPublic from which the ECC point is extracted. The
// trailing creation data/hash/ticket/name are consumed implicitly by not
// reading past outPublic — extracting the public point is sufficient for the
// attestation flow.
func parseCreatePrimaryResponse(rp []byte) (uint32, AKPublic, error) {
	handle, ok := common.GetU32(rp, 0)
	if !ok {
		return 0, AKPublic{}, common.ErrShortBuffer
	}
	// parameterSize (present for TagSessions responses): skip 4 bytes.
	if _, ok := common.GetU32(rp, 4); !ok {
		return 0, AKPublic{}, common.ErrShortBuffer
	}
	rest := rp[8:]
	// TPM2B_PUBLIC outPublic: a size-prefixed TPMT_PUBLIC.
	pubBytes, _, err := common.UnmarshalTPM2B(rest)
	if err != nil {
		return 0, AKPublic{}, err
	}
	pub, err := parseTPMTPublicECCPoint(pubBytes)
	if err != nil {
		return 0, AKPublic{}, err
	}
	return handle, pub, nil
}

// parseTPMTPublicECCPoint walks a TPMT_PUBLIC for an ECC key to its unique
// TPMS_ECC_POINT and returns the (x, y) coordinates. The fixed-shape prefix
// is consumed field by field so the parse is robust to the exact byte counts
// the TPM emitted (in particular the variable-length authPolicy). TCG "TPM
// 2.0 Part 2: Structures", clause "TPMT_PUBLIC" with parameters
// TPMS_ECC_PARMS and unique TPMS_ECC_POINT.
func parseTPMTPublicECCPoint(b []byte) (AKPublic, error) {
	off := 0
	// type (u16), nameAlg (u16): 4 bytes.
	if _, ok := common.GetU16(b, off); !ok {
		return AKPublic{}, common.ErrShortBuffer
	}
	off += 2
	if _, ok := common.GetU16(b, off); !ok {
		return AKPublic{}, common.ErrShortBuffer
	}
	off += 2
	// objectAttributes (u32).
	if _, ok := common.GetU32(b, off); !ok {
		return AKPublic{}, common.ErrShortBuffer
	}
	off += 4
	// authPolicy: TPM2B_DIGEST (skip it).
	_, after, err := common.UnmarshalTPM2B(b[off:])
	if err != nil {
		return AKPublic{}, err
	}
	// TPMS_ECC_PARMS: symmetric(u16) scheme(u16) [scheme.details if not NULL]
	// curveID(u16) kdf(u16). For the AK the scheme is ECDSA, whose details is
	// a single hashAlg (u16); symmetric=NULL and kdf=NULL carry no details.
	// We walk it generically: symmetric algorithm, then the scheme.
	p := after
	po := 0
	sym, ok := common.GetU16(p, po) // symmetric algorithm
	if !ok {
		return AKPublic{}, common.ErrShortBuffer
	}
	po += 2
	if sym != algNull {
		// A keyBits(u16)+mode(u16) would follow; not used by the AK, but
		// consume them to stay correct for symmetric != NULL.
		if _, ok := common.GetU16(p, po); !ok {
			return AKPublic{}, common.ErrShortBuffer
		}
		po += 2
		if _, ok := common.GetU16(p, po); !ok {
			return AKPublic{}, common.ErrShortBuffer
		}
		po += 2
	}
	scheme, ok := common.GetU16(p, po) // scheme algorithm
	if !ok {
		return AKPublic{}, common.ErrShortBuffer
	}
	po += 2
	if scheme != algNull {
		// scheme.details: a hashAlg (u16) for ECDSA/ECSCHNORR/etc.
		if _, ok := common.GetU16(p, po); !ok {
			return AKPublic{}, common.ErrShortBuffer
		}
		po += 2
	}
	// curveID (u16).
	if _, ok := common.GetU16(p, po); !ok {
		return AKPublic{}, common.ErrShortBuffer
	}
	po += 2
	// kdf scheme (u16).
	kdf, ok := common.GetU16(p, po)
	if !ok {
		return AKPublic{}, common.ErrShortBuffer
	}
	po += 2
	if kdf != algNull {
		// kdf.details: a hashAlg (u16).
		if _, ok := common.GetU16(p, po); !ok {
			return AKPublic{}, common.ErrShortBuffer
		}
		po += 2
	}
	// unique: TPMS_ECC_POINT { x: TPM2B, y: TPM2B }.
	x, rest2, err := common.UnmarshalTPM2B(p[po:])
	if err != nil {
		return AKPublic{}, err
	}
	y, _, err := common.UnmarshalTPM2B(rest2)
	if err != nil {
		return AKPublic{}, err
	}
	xc := make([]byte, len(x))
	copy(xc, x)
	yc := make([]byte, len(y))
	copy(yc, y)
	return AKPublic{X: xc, Y: yc}, nil
}

// ECDSASignature is a parsed TPMS_SIGNATURE_ECDSA: the (r, s) pair as
// big-endian byte strings. TCG "TPM 2.0 Part 2: Structures", clause
// "TPMS_SIGNATURE_ECDSA".
type ECDSASignature struct {
	// R is the big-endian r component.
	R []byte
	// S is the big-endian s component.
	S []byte
}

// Quote runs TPM2_Quote: the AK signs a TPMS_ATTEST that commits to the
// current values of the selected PCRs, with qualifyingData (a caller nonce)
// folded into the attest's extraData. It returns the raw TPM2B_ATTEST data
// (the bytes that were signed) and the parsed ECDSA signature.
//
// Body (TagSessions):
//
//	handle area: keyHandle (u32) = the AK handle from CreatePrimary
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_DATA qualifyingData || TPMT_SIG_SCHEME inScheme ||
//	             TPML_PCR_SELECTION pcrSelect
//
// inScheme is passed as TPM_ALG_NULL (0x0010) to use the key's own scheme
// (ECDSA+SHA256); a NULL scheme has no details, so it marshals as the bare
// 2-byte algorithm id.
//
// Wire: TagSessions, CC 0x00000158. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_Quote".
//
// Response:
//
//	param area: parameterSize (u32, present for TagSessions) ||
//	            TPM2B_ATTEST quoted || TPMT_SIGNATURE signature
func (tpm *TPM) Quote(keyHandle uint32, qualifyingData []byte, sel []PCRSelection) (quoted []byte, sig ECDSASignature, err error) {
	// Handle area.
	body := common.PutU32(nil, keyHandle)

	// Auth area.
	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	// Parameter area.
	body = append(body, common.MarshalTPM2B(qualifyingData)...) // TPM2B_DATA
	body = common.PutU16(body, algNull)                         // inScheme = NULL
	body = append(body, marshalPCRSelectionList(sel)...)        // pcrSelect

	rp, err := tpm.execute(common.TagSessions, common.CCQuote, body)
	if err != nil {
		return nil, ECDSASignature{}, err
	}
	return parseQuoteResponse(rp)
}

// parseQuoteResponse decodes the TPM2_Quote response: parameterSize (u32),
// TPM2B_ATTEST quoted, then a TPMT_SIGNATURE. For an ECDSA signature the
// TPMT_SIGNATURE is { sigAlg: TPM_ALG_ECDSA (u16), hash: TPM_ALG_SHA256
// (u16), signatureR: TPM2B, signatureS: TPM2B }. TCG "TPM 2.0 Part 2:
// Structures", clauses "TPMT_SIGNATURE" and "TPMS_SIGNATURE_ECDSA".
func parseQuoteResponse(rp []byte) ([]byte, ECDSASignature, error) {
	// parameterSize (TagSessions response).
	if _, ok := common.GetU32(rp, 0); !ok {
		return nil, ECDSASignature{}, common.ErrShortBuffer
	}
	rest := rp[4:]
	// TPM2B_ATTEST quoted.
	attest, rest, err := common.UnmarshalTPM2B(rest)
	if err != nil {
		return nil, ECDSASignature{}, err
	}
	quoted := make([]byte, len(attest))
	copy(quoted, attest)
	// TPMT_SIGNATURE: sigAlg (u16).
	sigAlg, ok := common.GetU16(rest, 0)
	if !ok {
		return nil, ECDSASignature{}, common.ErrShortBuffer
	}
	if sigAlg != AlgECDSA {
		return nil, ECDSASignature{}, ErrUnexpectedSigAlg
	}
	// hash (u16): the digest algorithm the signature was computed over.
	if _, ok := common.GetU16(rest, 2); !ok {
		return nil, ECDSASignature{}, common.ErrShortBuffer
	}
	// signatureR, signatureS: TPM2B each.
	r, rest2, err := common.UnmarshalTPM2B(rest[4:])
	if err != nil {
		return nil, ECDSASignature{}, err
	}
	s, _, err := common.UnmarshalTPM2B(rest2)
	if err != nil {
		return nil, ECDSASignature{}, err
	}
	rc := make([]byte, len(r))
	copy(rc, r)
	sc := make([]byte, len(s))
	copy(sc, s)
	return quoted, ECDSASignature{R: rc, S: sc}, nil
}

// ErrUnexpectedSigAlg is returned when a TPMT_SIGNATURE carries a signature
// algorithm other than the ECDSA this package decodes.
const ErrUnexpectedSigAlg = common.Error("tpm2: unexpected signature algorithm (want ECDSA)")
