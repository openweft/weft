// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import (
	"crypto/sha256"

	"github.com/go-tpm2/common"
)

// This file computes an object's Name, the cryptographic identity the TPM
// binds to a loaded object. The Name is what MakeCredential commits to (it is
// folded into the KDFa contexts and the outer HMAC), so the credential a
// verifier produces can only be recovered by a TPM that holds an object with
// exactly this Name. TCG "TPM 2.0 Part 1: Architecture", clause "Names";
// "Part 2: Structures", clause "TPM2B_NAME".

// ObjectName computes the Name of an object from its public area (the
// TPMT_PUBLIC bytes, i.e. the CONTENTS of the TPM2B_PUBLIC, without the
// 2-byte size prefix). For an object whose nameAlg is a hash algorithm, the
// Name is:
//
//	Name = nameAlg (UINT16, big-endian) || H_nameAlg( TPMT_PUBLIC )
//
// where the hash covers the entire marshaled TPMT_PUBLIC. The nameAlg is the
// second UINT16 of the public area (after the 2-byte `type`). This package
// supports the SHA-256 nameAlg used by the AK/EK templates; a public area
// with any other nameAlg is rejected. TCG "Part 1", "Names" (the Name of a
// loaded object); "Part 2", "TPMT_PUBLIC" (field order) and "TPM2B_NAME".
//
// The returned Name is the bare (algorithm-id || digest) value — NOT wrapped
// in a TPM2B. Callers that need a TPM2B_NAME wrap it with common.MarshalTPM2B.
func ObjectName(publicArea []byte) ([]byte, error) {
	nameAlg, ok := common.GetU16(publicArea, 2) // skip type (u16), read nameAlg
	if !ok {
		return nil, common.ErrShortBuffer
	}
	if nameAlg != algSHA256 {
		return nil, ErrUnsupportedNameAlg
	}
	digest := sha256.Sum256(publicArea)
	name := common.PutU16(nil, nameAlg)
	name = append(name, digest[:]...)
	return name, nil
}

// ErrUnsupportedNameAlg is returned by ObjectName when the public area's
// nameAlg is not one this package can compute a Name for (only SHA-256 is
// supported, matching the AK/EK templates).
const ErrUnsupportedNameAlg = common.Error("tpm2: unsupported nameAlg for ObjectName (want SHA-256)")
