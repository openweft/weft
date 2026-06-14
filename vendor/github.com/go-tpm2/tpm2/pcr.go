// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import "github.com/go-tpm2/common"

// pcrSelectSize is the byte length of the pcrSelect bitmap used throughout
// this package. The TPM 2.0 wire format makes the bitmap length explicit
// (the sizeofSelect octet), but PCR[0..23] — the architecturally defined
// platform PCRs — fit in three octets, so this stack always emits a
// three-octet selection. TCG "TPM 2.0 Part 2: Structures", clause
// "TPMS_PCR_SELECTION".
const pcrSelectSize = 3

// PCRSelection names a set of PCRs within one PCR bank, identified by its
// hash algorithm. It is the typed form of the TPMS_PCR_SELECTION wire
// structure { hash, sizeofSelect, pcrSelect[] }. TCG "TPM 2.0 Part 2:
// Structures", clause "TPMS_PCR_SELECTION".
type PCRSelection struct {
	// Hash is the bank's hash algorithm (a TPM_ALG_ID value, e.g.
	// common.AlgSHA256).
	Hash uint16
	// PCRs is the list of PCR indices selected in this bank.
	PCRs []int
}

// bitmap renders the selected PCR indices as the little-endian,
// octet-indexed pcrSelect bitmap the wire format uses: PCR n is bit (n mod
// 8) of octet (n div 8), least-significant bit first within each octet.
// Indices that fall outside the three-octet (PCR[0..23]) window are
// ignored. TCG "TPM 2.0 Part 2: Structures", clause "TPMS_PCR_SELECTION".
func (s PCRSelection) bitmap() []byte {
	b := make([]byte, pcrSelectSize)
	for _, pcr := range s.PCRs {
		if pcr < 0 || pcr >= pcrSelectSize*8 {
			continue
		}
		b[pcr/8] |= 1 << uint(pcr%8)
	}
	return b
}

// pcrsFromBitmap is the inverse of bitmap: it expands an octet-indexed
// pcrSelect bitmap into the ascending list of selected PCR indices. It is
// used to decode the TPML_PCR_SELECTION the TPM echoes back from PCR_Read.
func pcrsFromBitmap(b []byte) []int {
	var out []int
	for i, octet := range b {
		for bit := 0; bit < 8; bit++ {
			if octet&(1<<uint(bit)) != 0 {
				out = append(out, i*8+bit)
			}
		}
	}
	return out
}

// marshalPCRSelectionList marshals sel as a TPML_PCR_SELECTION:
//
//	[ count:u32 | TPMS_PCR_SELECTION... ]
//
// where each TPMS_PCR_SELECTION is
//
//	[ hash:u16 | sizeofSelect:u8 | pcrSelect[sizeofSelect] ].
//
// TCG "TPM 2.0 Part 2: Structures", clauses "TPML_PCR_SELECTION" and
// "TPMS_PCR_SELECTION".
func marshalPCRSelectionList(sel []PCRSelection) []byte {
	out := common.PutU32(nil, uint32(len(sel)))
	for _, s := range sel {
		out = common.PutU16(out, s.Hash)
		out = common.PutU8(out, pcrSelectSize)
		out = append(out, s.bitmap()...)
	}
	return out
}

// parsePCRSelectionList decodes a TPML_PCR_SELECTION from the front of b
// and returns the decoded selections and the remaining bytes. It is used to
// skip over the selection list the TPM echoes in the PCR_Read response.
func parsePCRSelectionList(b []byte) (sel []PCRSelection, rest []byte, err error) {
	count, ok := common.GetU32(b, 0)
	if !ok {
		return nil, nil, common.ErrShortBuffer
	}
	off := 4
	for i := uint32(0); i < count; i++ {
		hash, ok := common.GetU16(b, off)
		if !ok {
			return nil, nil, common.ErrShortBuffer
		}
		sizeOfSelect, ok := common.GetU8(b, off+2)
		if !ok {
			return nil, nil, common.ErrShortBuffer
		}
		start := off + 3
		end := start + int(sizeOfSelect)
		if end > len(b) {
			return nil, nil, common.ErrShortBuffer
		}
		sel = append(sel, PCRSelection{
			Hash: hash,
			PCRs: pcrsFromBitmap(b[start:end]),
		})
		off = end
	}
	return sel, b[off:], nil
}
