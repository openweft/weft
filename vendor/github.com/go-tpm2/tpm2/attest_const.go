// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

// Algorithm identifiers used by the attestation flow that are not already in
// common's starter set. Values are TPM_ALG_ID per TCG "TPM 2.0 Part 2:
// Structures", clause "TPM_ALG_ID", as registered in the TCG "Algorithm
// Registry" (the "Part 4" algorithm IDs). They are encoded big-endian as
// UINT16 on the wire.
const (
	// AlgRSA is TPM_ALG_RSA (an object/asymmetric algorithm). Unused by the
	// ECC AK flow but listed for completeness of the registry transcription.
	AlgRSA uint16 = 0x0001
	// AlgECDSA is TPM_ALG_ECDSA, the ECC signing scheme used by the AK.
	AlgECDSA uint16 = 0x0018
	// AlgECC is TPM_ALG_ECC, the object type of an ECC key.
	AlgECC uint16 = 0x0023
	// AlgSHA256 mirrors common.AlgSHA256 (0x000B) as a bare uint16 for the
	// marshaling helpers in this package.
	algSHA256 uint16 = 0x000B
	// algNull mirrors common.AlgNull (0x0010): TPM_ALG_NULL, the "no
	// algorithm" selector used for symmetric and kdf of a signing-only ECC
	// key.
	algNull uint16 = 0x0010
)

// TPM_ECC_CURVE identifiers. TCG "TPM 2.0 Part 2: Structures", clause
// "TPM_ECC_CURVE"; the registered curve values are in the TCG "Algorithm
// Registry". Encoded big-endian as UINT16.
const (
	// ECCNistP256 is TPM_ECC_NIST_P256, the NIST P-256 (secp256r1) curve.
	ECCNistP256 uint16 = 0x0003
)

// TPMA_OBJECT attribute bits. TCG "TPM 2.0 Part 2: Structures", clause
// "TPMA_OBJECT (Object Attributes)". The attribute set is a UINT32 bit field
// (encoded big-endian). The bits below are the ones an ECC restricted
// signing Attestation Key sets.
const (
	// objFixedTPM (bit 1): the object cannot be duplicated to a different
	// parent / TPM.
	objFixedTPM uint32 = 1 << 1
	// objFixedParent (bit 4): the object's parent cannot be changed.
	objFixedParent uint32 = 1 << 4
	// objSensitiveDataOrigin (bit 5): the TPM generated the sensitive data
	// (the private key), not the caller.
	objSensitiveDataOrigin uint32 = 1 << 5
	// objUserWithAuth (bit 6): the userAuth value may authorize use of the
	// object with a password/HMAC session.
	objUserWithAuth uint32 = 1 << 6
	// objRestricted (bit 16): a restricted key. A restricted signing key may
	// only sign TPM-generated data (e.g. a TPMS_ATTEST), which is exactly
	// what makes it an Attestation Key.
	objRestricted uint32 = 1 << 16
	// objSign (bit 18): the key may be used to sign (and, for a restricted
	// key, to attest).
	objSign uint32 = 1 << 18

	// akObjectAttributes is the TPMA_OBJECT for the ECC restricted signing
	// Attestation Key:
	//
	//	fixedTPM | fixedParent | sensitiveDataOrigin |
	//	userWithAuth | restricted | sign
	//
	//	= (1<<1)|(1<<4)|(1<<5)|(1<<6)|(1<<16)|(1<<18)
	//	= 0x02 | 0x10 | 0x20 | 0x40 | 0x10000 | 0x40000
	//	= 0x00050072
	akObjectAttributes = objFixedTPM | objFixedParent | objSensitiveDataOrigin |
		objUserWithAuth | objRestricted | objSign
)

// Attestation structure constants. TCG "TPM 2.0 Part 2: Structures".
const (
	// attestMagic is TPM_GENERATED_VALUE, the leading UINT32 of every
	// TPMS_ATTEST. A genuine TPM always prefixes attestation data with this
	// value; verifiers reject any attest that lacks it. Clause
	// "TPM_GENERATED".
	attestMagic uint32 = 0xFF544347
	// stAttestQuote is TPM_ST_ATTEST_QUOTE, the structure tag identifying a
	// TPMS_ATTEST whose union member is a TPMS_QUOTE_INFO. Clause "TPM_ST
	// (Structure Tags)".
	stAttestQuote uint16 = 0x8018
)
