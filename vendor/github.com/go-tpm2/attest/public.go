// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

import "github.com/go-tpm2/common"

// This file parses the ECC public point (x, y) out of a TPMT_PUBLIC public
// area. The verifier needs the AK's and EK's affine coordinates to run quote
// verification and MakeCredential, but holds only the TPMT_PUBLIC bytes (the
// value it must also hash to recompute the object Name).
//
// tpm2 v0.5.0 derives these points internally (parseTPMTPublicECCPoint) but
// does NOT export a TPMT_PUBLIC -> point function — only the typed AKPublic /
// EKPublic structs and the commands that produce them. Rather than change the
// AdmissionRequest/registry to carry pre-parsed coordinates (which would let a
// peer assert a point that disagrees with the AK Name it enrolled), this
// package re-derives the point from the same bytes the Name is computed over.
// That keeps a single source of truth: the TPMT_PUBLIC.
//
// The walk mirrors the field order of an ECC TPMT_PUBLIC. TCG "TPM 2.0 Part 2:
// Structures", clause "TPMT_PUBLIC" with parameters TPMS_ECC_PARMS and unique
// TPMS_ECC_POINT.

// algNull is TPM_ALG_NULL, the "no algorithm" marker that makes a scheme/kdf/
// symmetric field carry no following details.
const algNull = 0x0010

// eccPoint is the affine (x, y) of an ECC public point, big-endian.
type eccPoint struct {
	X []byte
	Y []byte
}

// ErrBadPublic is returned when a public area is not a parseable ECC
// TPMT_PUBLIC.
const ErrBadPublic = common.Error("attest: malformed ECC TPMT_PUBLIC")

// parseECCPoint extracts the unique TPMS_ECC_POINT (x, y) from an ECC
// TPMT_PUBLIC. The fixed-shape prefix is consumed field by field so the parse
// is robust to the exact byte counts the TPM emitted (in particular the
// variable-length authPolicy and the optional scheme/kdf/symmetric details).
func parseECCPoint(b []byte) (eccPoint, error) {
	off := 0
	// type (u16), nameAlg (u16): 4 bytes.
	if _, ok := common.GetU16(b, off); !ok {
		return eccPoint{}, ErrBadPublic
	}
	off += 2
	if _, ok := common.GetU16(b, off); !ok {
		return eccPoint{}, ErrBadPublic
	}
	off += 2
	// objectAttributes (u32).
	if _, ok := common.GetU32(b, off); !ok {
		return eccPoint{}, ErrBadPublic
	}
	off += 4
	// authPolicy: TPM2B_DIGEST (skip it).
	_, after, err := common.UnmarshalTPM2B(b[off:])
	if err != nil {
		return eccPoint{}, ErrBadPublic
	}
	// TPMS_ECC_PARMS: symmetric, scheme, curveID, kdf — each possibly with
	// trailing details when not NULL.
	p := after
	po := 0
	sym, ok := common.GetU16(p, po) // symmetric algorithm
	if !ok {
		return eccPoint{}, ErrBadPublic
	}
	po += 2
	if sym != algNull {
		// keyBits(u16) + mode(u16) for a real symmetric cipher (the EK's AES).
		if _, ok := common.GetU16(p, po); !ok {
			return eccPoint{}, ErrBadPublic
		}
		po += 2
		if _, ok := common.GetU16(p, po); !ok {
			return eccPoint{}, ErrBadPublic
		}
		po += 2
	}
	scheme, ok := common.GetU16(p, po) // scheme algorithm
	if !ok {
		return eccPoint{}, ErrBadPublic
	}
	po += 2
	if scheme != algNull {
		// scheme.details: a hashAlg (u16) for ECDSA.
		if _, ok := common.GetU16(p, po); !ok {
			return eccPoint{}, ErrBadPublic
		}
		po += 2
	}
	// curveID (u16).
	if _, ok := common.GetU16(p, po); !ok {
		return eccPoint{}, ErrBadPublic
	}
	po += 2
	// kdf scheme (u16).
	kdf, ok := common.GetU16(p, po)
	if !ok {
		return eccPoint{}, ErrBadPublic
	}
	po += 2
	if kdf != algNull {
		// kdf.details: a hashAlg (u16).
		if _, ok := common.GetU16(p, po); !ok {
			return eccPoint{}, ErrBadPublic
		}
		po += 2
	}
	// unique: TPMS_ECC_POINT { x: TPM2B, y: TPM2B }.
	x, rest2, err := common.UnmarshalTPM2B(p[po:])
	if err != nil {
		return eccPoint{}, ErrBadPublic
	}
	y, _, err := common.UnmarshalTPM2B(rest2)
	if err != nil {
		return eccPoint{}, ErrBadPublic
	}
	xc := make([]byte, len(x))
	copy(xc, x)
	yc := make([]byte, len(y))
	copy(yc, y)
	return eccPoint{X: xc, Y: yc}, nil
}
