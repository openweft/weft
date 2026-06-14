// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import (
	"crypto/hmac"
	"crypto/sha256"

	"github.com/go-tpm2/common"
)

// This file implements the two TPM 2.0 key-derivation functions used by
// Credential Protection (MakeCredential / ActivateCredential): KDFa and
// KDFe. Both are defined in TCG "TPM 2.0 Part 1: Architecture", clause
// "Key Derivation Functions". This package only needs the SHA-256 instances,
// so they are hard-wired to SHA-256 (the EK Credential Profile's nameAlg).

// labelBytes renders a KDF Label as the TPM does: the ASCII bytes of the
// label FOLLOWED BY a single terminating 0x00 octet. TCG "Part 1", "Key
// Derivation Functions" specifies the label is "a NULL-terminated string";
// the trailing null is part of the hashed/HMACed input. A label that already
// ends in 0x00 is not double-terminated.
func labelBytes(label string) []byte {
	b := []byte(label)
	if len(b) == 0 || b[len(b)-1] != 0x00 {
		b = append(b, 0x00)
	}
	return b
}

// KDFa implements the TPM 2.0 SP800-108 counter-mode HMAC KDF (KDFa) with
// SHA-256 as the hash. It derives `bits` bits of key material from `key`,
// bound to a textual `label` and a (contextU || contextV) context.
//
// Per TCG "TPM 2.0 Part 1: Architecture", "Key Derivation Functions"
// (KDFa()):
//
//	KDFa(hashAlg, key, label, contextU, contextV, bits):
//	  bytes = ceil(bits / 8)
//	  for i = 1, 2, ... until `bytes` produced:
//	    K_i = HMAC(key, BE32(i)            ||  // the SP800-108 counter
//	                    label || 0x00      ||  // Label, NULL-terminated
//	                    contextU           ||  // PartyUInfo
//	                    contextV           ||  // PartyVInfo
//	                    BE32(bits))            // the requested bit length
//	  result = (K_1 || K_2 || ...) truncated to `bytes`
//
// When `bits` is not a whole number of bytes the leftmost `bits` bits are
// kept and the excess low-order bits of the last produced byte are masked to
// zero (TCG "Part 1": "the resulting string of bits is the least number of
// bytes ... the bits not used are zeroed"). For the 128- and 256-bit uses in
// Credential Protection `bits` is byte-aligned, but the masking is
// implemented for completeness/correctness.
func KDFa(key []byte, label string, contextU, contextV []byte, bits int) []byte {
	lab := labelBytes(label)
	bitsBE := common.PutU32(nil, uint32(bits))
	outLen := (bits + 7) / 8

	var out []byte
	for ctr := uint32(1); len(out) < outLen; ctr++ {
		h := hmac.New(sha256.New, key)
		h.Write(common.PutU32(nil, ctr)) // counter i
		h.Write(lab)                     // label || 0x00
		h.Write(contextU)                // PartyUInfo
		h.Write(contextV)                // PartyVInfo
		h.Write(bitsBE)                  // bit length
		out = append(out, h.Sum(nil)...)
	}
	out = out[:outLen]
	// Mask the excess low-order bits of the final byte when bits is not a
	// whole number of bytes.
	if rem := bits % 8; rem != 0 {
		out[len(out)-1] &= byte(0xFF << uint(8-rem))
	}
	return out
}

// KDFe implements the TPM 2.0 SP800-56A concatenation KDF (KDFe) with
// SHA-256 as the hash. It derives `bits` bits of shared key material from the
// ECDH shared secret `z` (the x-coordinate of the shared point), bound to a
// textual `label` and the two parties' contributions partyU and partyV.
//
// Per TCG "TPM 2.0 Part 1: Architecture", "Key Derivation Functions"
// (KDFe()):
//
//	KDFe(hashAlg, Z, label, partyUInfo, partyVInfo, bits):
//	  bytes = ceil(bits / 8)
//	  for counter = 1, 2, ... until `bytes` produced:
//	    K_i = H(BE32(counter)        ||  // SP800-56A counter, starts at 1
//	            Z                    ||  // the ECDH shared secret (point x)
//	            label || 0x00        ||  // the NULL-terminated label
//	            partyUInfo           ||  // initiator contribution
//	            partyVInfo)              // responder contribution
//	  result = (K_1 || K_2 || ...) truncated to `bytes`
//
// For Credential Protection the seed is a single SHA-256 block (256 bits),
// so exactly one iteration runs; the loop and masking handle the general
// case.
func KDFe(z []byte, label string, partyU, partyV []byte, bits int) []byte {
	lab := labelBytes(label)
	outLen := (bits + 7) / 8

	var out []byte
	for ctr := uint32(1); len(out) < outLen; ctr++ {
		h := sha256.New()
		h.Write(common.PutU32(nil, ctr)) // counter
		h.Write(z)                       // Z (ECDH shared secret)
		h.Write(lab)                     // label || 0x00
		h.Write(partyU)                  // PartyUInfo
		h.Write(partyV)                  // PartyVInfo
		out = append(out, h.Sum(nil)...)
	}
	out = out[:outLen]
	if rem := bits % 8; rem != 0 {
		out[len(out)-1] &= byte(0xFF << uint(8-rem))
	}
	return out
}
