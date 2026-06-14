// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"math/big"

	"github.com/go-tpm2/common"
)

// This file implements the VERIFIER's side of TPM 2.0 Credential Protection:
// MakeCredential, computed entirely OFF the TPM in pure Go from the EK's
// public key. It produces the (credentialBlob, secret) pair that the TPM's
// TPM2_ActivateCredential recovers only if the activate handle (the AK) has
// the Name committed to here AND the key handle (the EK) is the private peer
// of the EK public used here. That binding — AK and EK on the SAME TPM — is
// the identity step of remote attestation.
//
// For an ECC Endorsement Key the construction is the "secret sharing" /
// outer-wrap of TCG "TPM 2.0 Part 1: Architecture", clause "Credential
// Protection" (and the shared "Protected Storage" outer-wrap primitives it
// references). With SHA-256 as the EK nameAlg and AES-128-CFB as the EK
// symmetric:
//
//	(de, Qe) := ephemeral P-256 key pair                       // the "secret"
//	Z        := x-coord of de * EK_pub                          // ECDH shared
//	seed     := KDFe(SHA256, Z, "IDENTITY", Qe.x, EK.x, 256)    // 32-byte seed
//	symKey   := KDFa(SHA256, seed, "STORAGE",  AKname, nil, 128)
//	encIdentity := AES128-CFB(symKey, IV=0) over TPM2B(credential)
//	HMACkey  := KDFa(SHA256, seed, "INTEGRITY", nil, nil, 256)
//	outerHMAC := HMAC-SHA256(HMACkey, encIdentity || AKname)
//	credentialBlob := TPM2B_ID_OBJECT { TPM2B(outerHMAC) || encIdentity }
//	secret         := TPM2B_ENCRYPTED_SECRET( TPMS_ECC_POINT{ Qe.x, Qe.y } )
//
// The KDF LABELS and the WRAP ORDER are the most error-prone part; the labels
// are taken verbatim from TCG "Part 1": "IDENTITY" (the seed derivation for
// an activation/identity object), "STORAGE" (the symmetric-key derivation of
// the outer wrap), "INTEGRITY" (the outer-HMAC-key derivation). The outer
// HMAC is computed over (encIdentity || name) — ciphertext first, then the
// object Name — exactly as the TPM checks it in ActivateCredential. TCG
// "Part 1", "Credential Protection"; "Protected Storage" (KDFa/KDFe labels
// and the outer integrity wrap); "Part 2: Structures", clauses
// "TPM2B_ID_OBJECT" and "TPM2B_ENCRYPTED_SECRET" / "TPMS_ECC_POINT".

// Credential Protection KDF labels (NULL-terminated by the KDF helpers). TCG
// "TPM 2.0 Part 1: Architecture", "Credential Protection" / "Protected
// Storage".
const (
	// labelIdentity is the KDFe label for the activation seed.
	labelIdentity = "IDENTITY"
	// labelStorage is the KDFa label for the outer-wrap symmetric key.
	labelStorage = "STORAGE"
	// labelIntegrity is the KDFa label for the outer-wrap HMAC key.
	labelIntegrity = "INTEGRITY"
)

// p256FieldBytes is the fixed width (in bytes) of a NIST P-256 field element:
// coordinates and the ECDH Z value are always left-zero-padded to 32 bytes
// before being marshaled or fed to a KDF. TCG "Part 1" requires fixed-width
// (key-size) buffers for the KDF context/secret inputs.
const p256FieldBytes = 32

// MakeCredentialResult is the output of MakeCredential: the two blobs a
// verifier hands to the attesting platform for TPM2_ActivateCredential.
type MakeCredentialResult struct {
	// CredentialBlob is the TPM2B_ID_OBJECT { integrityHMAC || encIdentity }
	// (the size-prefixed id-object), ready to pass as ActivateCredential's
	// credentialBlob parameter.
	CredentialBlob []byte
	// Secret is the TPM2B_ENCRYPTED_SECRET carrying the ephemeral public point
	// (TPMS_ECC_POINT), ready to pass as ActivateCredential's secret parameter.
	Secret []byte
}

// MakeCredential performs the off-TPM verifier side of Credential Protection
// for an ECC P-256 Endorsement Key. ekPublic is the EK's public point,
// akName is the loaded AK's Name (from ObjectName), and credential is the
// secret challenge to bind (a TPM2B_DIGEST payload, at most 32 bytes for the
// SHA-256 scheme). It returns the credentialBlob and secret to feed to
// TPM2_ActivateCredential.
//
// The ephemeral key is drawn from crypto/rand; rng may override the source
// for deterministic testing (pass nil for crypto/rand).
func MakeCredential(ekPublic EKPublic, akName, credential []byte, rng io.Reader) (MakeCredentialResult, error) {
	if rng == nil {
		rng = rand.Reader
	}
	curve := elliptic.P256()

	// Validate that the EK public point is on the curve before using it.
	ekX := new(big.Int).SetBytes(ekPublic.X)
	ekY := new(big.Int).SetBytes(ekPublic.Y)
	if !curve.IsOnCurve(ekX, ekY) {
		return MakeCredentialResult{}, ErrEKPointNotOnCurve
	}

	// Ephemeral P-256 key pair (the "secret"): de private, Qe = de*G public.
	eph, err := ecdsa.GenerateKey(curve, rng)
	if err != nil {
		return MakeCredentialResult{}, err
	}

	// ECDH: Z = x-coord of de * EK_pub. Fixed-width 32-byte big-endian.
	zx, _ := curve.ScalarMult(ekX, ekY, eph.D.Bytes())
	z := leftPad(zx.Bytes(), p256FieldBytes)

	// Fixed-width coordinates for the KDFe context and the secret point.
	qeX := leftPad(eph.X.Bytes(), p256FieldBytes)
	qeY := leftPad(eph.Y.Bytes(), p256FieldBytes)
	ekXfix := leftPad(ekPublic.X, p256FieldBytes)

	// seed = KDFe(Z, "IDENTITY", partyU=Qe.x, partyV=EK.x, 256).
	seed := KDFe(z, labelIdentity, qeX, ekXfix, 256)

	// symKey = KDFa(seed, "STORAGE", context=AKname, nil, 128). KDFa(_,128)
	// always yields a 16-byte key, so AES-128 construction below cannot fail.
	symKey := KDFa(seed, labelStorage, akName, nil, 128)

	// encIdentity = AES-128-CFB(symKey, IV=0) over TPM2B(credential).
	plain := common.MarshalTPM2B(credential)
	encIdentity := aesCFBZeroIV(symKey, plain)

	// HMACkey = KDFa(seed, "INTEGRITY", nil, nil, 256).
	hmacKey := KDFa(seed, labelIntegrity, nil, nil, 256)

	// outerHMAC = HMAC-SHA256(HMACkey, encIdentity || AKname).
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(encIdentity)
	mac.Write(akName)
	outerHMAC := mac.Sum(nil)

	// credentialBlob = TPM2B_ID_OBJECT { TPM2B(outerHMAC) || encIdentity }.
	idObject := common.MarshalTPM2B(outerHMAC)
	idObject = append(idObject, encIdentity...)
	credentialBlob := common.MarshalTPM2B(idObject)

	// secret = TPM2B_ENCRYPTED_SECRET( TPMS_ECC_POINT{ x: Qe.x, y: Qe.y } ).
	point := common.MarshalTPM2B(qeX)
	point = append(point, common.MarshalTPM2B(qeY)...)
	secret := common.MarshalTPM2B(point)

	return MakeCredentialResult{CredentialBlob: credentialBlob, Secret: secret}, nil
}

// aesCFBZeroIV encrypts plaintext with AES in CFB mode using an all-zero IV
// (the TPM uses a zero IV for the outer-wrap symmetric encryption of an id
// object). key MUST be exactly a 16-byte (AES-128) key — Credential
// Protection for the L-2 EK derives it via KDFa(_,128), which always returns
// 16 bytes — so aes.NewCipher cannot fail and its error is discarded. TCG
// "Part 1", "Protected Storage" (symmetric outer wrap, CFB, IV of zero).
func aesCFBZeroIV(key, plaintext []byte) []byte {
	block, _ := aes.NewCipher(key) // key is a guaranteed-valid 16-byte AES key
	iv := make([]byte, block.BlockSize())
	out := make([]byte, len(plaintext))
	cipher.NewCFBEncrypter(block, iv).XORKeyStream(out, plaintext)
	return out
}

// leftPad returns b left-padded with zero bytes to exactly n bytes. If b is
// already n bytes it is returned as a fresh copy; if longer, its low-order n
// bytes are kept (this does not occur for valid P-256 coordinates).
func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		out := make([]byte, n)
		copy(out, b[len(b)-n:])
		return out
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

// ErrEKPointNotOnCurve is returned by MakeCredential when the supplied EK
// public point does not lie on NIST P-256 (a malformed or wrong EK public).
const ErrEKPointNotOnCurve = common.Error("tpm2: EK public point is not on the P-256 curve")
