// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"math/big"

	"github.com/go-tpm2/common"
)

// QuoteInfo is the parsed TPMS_QUOTE_INFO carried in a quote attestation: the
// set of PCRs that were quoted and the digest the TPM computed over their
// values. TCG "TPM 2.0 Part 2: Structures", clause "TPMS_QUOTE_INFO".
type QuoteInfo struct {
	// PCRSelect is the TPML_PCR_SELECTION the TPM committed to.
	PCRSelect []PCRSelection
	// PCRDigest is the TPM2B_DIGEST: H(concatenation of selected PCR values).
	PCRDigest []byte
}

// AttestInfo is the parsed, attestation-relevant subset of a TPMS_ATTEST of
// type TPM_ST_ATTEST_QUOTE. TCG "TPM 2.0 Part 2: Structures", clause
// "TPMS_ATTEST".
type AttestInfo struct {
	// ExtraData echoes the caller's qualifyingData nonce (the TPM2B_DATA
	// extraData field), so a verifier can bind the quote to its challenge.
	ExtraData []byte
	// Quote is the TPMS_QUOTE_INFO union member.
	Quote QuoteInfo
}

// Attestation/verification error sentinels (typed and constant for ==).
const (
	// ErrBadMagic is returned when a TPMS_ATTEST does not begin with
	// TPM_GENERATED_VALUE (0xFF544347).
	ErrBadMagic = common.Error("tpm2: attest magic is not TPM_GENERATED_VALUE")
	// ErrNotQuote is returned when a TPMS_ATTEST is not a TPM_ST_ATTEST_QUOTE.
	ErrNotQuote = common.Error("tpm2: attest type is not TPM_ST_ATTEST_QUOTE")
	// ErrSigInvalid is returned when the ECDSA signature over the attest does
	// not verify against the AK public key.
	ErrSigInvalid = common.Error("tpm2: ECDSA signature does not verify")
	// ErrPCRDigestMismatch is returned when the quoted pcrDigest does not
	// equal the SHA-256 of the concatenated expected PCR values.
	ErrPCRDigestMismatch = common.Error("tpm2: quoted pcrDigest != recomputed PCR digest")
)

// ParseAttest decodes a TPMS_ATTEST of type TPM_ST_ATTEST_QUOTE from the raw
// TPM2B_ATTEST data (i.e. the bytes that were signed, NOT including the
// TPM2B size prefix). The fixed prefix is:
//
//	magic            : UINT32  (must be 0xFF544347)
//	type             : TPM_ST  (UINT16; must be 0x8018 ATTEST_QUOTE)
//	qualifiedSigner  : TPM2B_NAME
//	extraData        : TPM2B_DATA
//	clockInfo        : TPMS_CLOCK_INFO = clock:u64, resetCount:u32,
//	                                     restartCount:u32, safe:u8  (17 bytes)
//	firmwareVersion  : UINT64
//	attested         : TPMS_QUOTE_INFO = TPML_PCR_SELECTION pcrSelect,
//	                                      TPM2B_DIGEST pcrDigest
//
// TCG "TPM 2.0 Part 2: Structures", clauses "TPMS_ATTEST", "TPMS_CLOCK_INFO",
// "TPMS_QUOTE_INFO".
func ParseAttest(data []byte) (AttestInfo, error) {
	magic, ok := common.GetU32(data, 0)
	if !ok {
		return AttestInfo{}, common.ErrShortBuffer
	}
	if magic != attestMagic {
		return AttestInfo{}, ErrBadMagic
	}
	typ, ok := common.GetU16(data, 4)
	if !ok {
		return AttestInfo{}, common.ErrShortBuffer
	}
	if typ != stAttestQuote {
		return AttestInfo{}, ErrNotQuote
	}
	off := 6
	// qualifiedSigner: TPM2B_NAME (skip).
	_, after, err := common.UnmarshalTPM2B(data[off:])
	if err != nil {
		return AttestInfo{}, err
	}
	// extraData: TPM2B_DATA (keep).
	extra, after, err := common.UnmarshalTPM2B(after)
	if err != nil {
		return AttestInfo{}, err
	}
	extraCopy := make([]byte, len(extra))
	copy(extraCopy, extra)
	// clockInfo (17 bytes) + firmwareVersion (8 bytes) = 25 fixed bytes.
	if len(after) < 25 {
		return AttestInfo{}, common.ErrShortBuffer
	}
	after = after[25:]
	// attested = TPMS_QUOTE_INFO: TPML_PCR_SELECTION then TPM2B_DIGEST.
	sel, after, err := parsePCRSelectionList(after)
	if err != nil {
		return AttestInfo{}, err
	}
	digest, _, err := common.UnmarshalTPM2B(after)
	if err != nil {
		return AttestInfo{}, err
	}
	digestCopy := make([]byte, len(digest))
	copy(digestCopy, digest)
	return AttestInfo{
		ExtraData: extraCopy,
		Quote: QuoteInfo{
			PCRSelect: sel,
			PCRDigest: digestCopy,
		},
	}, nil
}

// VerifyQuote performs the full attestation check for an ECDSA-P256 AK:
//
//  1. parse the TPMS_ATTEST from quoted (the TPM2B_ATTEST.data) and confirm
//     it is a genuine TPM_GENERATED quote;
//  2. verify the ECDSA signature (sig.R, sig.S) over SHA-256(quoted) using
//     the AK public point (akPub.X, akPub.Y) on NIST P-256;
//  3. recompute the PCR digest as SHA-256(expectedPCRs[0] || expectedPCRs[1]
//     || ...) and confirm it equals the pcrDigest the TPM committed to.
//
// expectedPCRs must be the selected PCR values, in the TPM's ascending
// selection order, exactly as PCRRead returns them. On success it returns the
// parsed AttestInfo (so callers can also check extraData against their nonce).
//
// All cryptography is pure-Go (crypto/ecdsa + crypto/elliptic P-256), so the
// helper is usable from a tamago guest with CGO disabled.
func VerifyQuote(akPub AKPublic, quoted []byte, sig ECDSASignature, expectedPCRs [][]byte) (AttestInfo, error) {
	info, err := ParseAttest(quoted)
	if err != nil {
		return AttestInfo{}, err
	}

	// (a) Verify the ECDSA signature over SHA-256(quoted).
	curve := elliptic.P256()
	pub := &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(akPub.X),
		Y:     new(big.Int).SetBytes(akPub.Y),
	}
	digest := sha256.Sum256(quoted)
	r := new(big.Int).SetBytes(sig.R)
	s := new(big.Int).SetBytes(sig.S)
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return AttestInfo{}, ErrSigInvalid
	}

	// (b) Recompute the PCR digest and compare.
	h := sha256.New()
	for _, v := range expectedPCRs {
		h.Write(v)
	}
	recomputed := h.Sum(nil)
	if !bytesEqual(recomputed, info.Quote.PCRDigest) {
		return AttestInfo{}, ErrPCRDigestMismatch
	}
	return info, nil
}

// bytesEqual reports whether a and b have the same length and contents.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
