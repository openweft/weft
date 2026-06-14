// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import "github.com/go-tpm2/common"

// This file implements the NV (Non-Volatile) storage commands, authorized
// under the OWNER hierarchy with a cleartext-password (RS_PW) session over the
// empty owner authValue: TPM2_NV_DefineSpace, TPM2_NV_Write, TPM2_NV_Read,
// TPM2_NV_ReadPublic, and TPM2_NV_UndefineSpace. TCG "TPM 2.0 Part 3:
// Commands", clauses "TPM2_NV_*"; the NV public area shape is "TPM 2.0 Part 2:
// Structures", clause "TPMS_NV_PUBLIC" / "TPM2B_NV_PUBLIC".

// Command codes for the NV flow. common v0.1.0 predates this milestone, so
// they are defined here with the TPM_CC values the TCG registry assigns. TCG
// "TPM 2.0 Part 2: Structures", clause "TPM_CC (Command Codes)".
const (
	// ccNVDefineSpace is TPM2_NV_DefineSpace.
	ccNVDefineSpace common.TPM_CC = 0x0000012A
	// ccNVUndefineSpace is TPM2_NV_UndefineSpace.
	ccNVUndefineSpace common.TPM_CC = 0x00000122
	// ccNVWrite is TPM2_NV_Write.
	ccNVWrite common.TPM_CC = 0x00000137
	// ccNVRead is TPM2_NV_Read.
	ccNVRead common.TPM_CC = 0x0000014E
	// ccNVReadPublic is TPM2_NV_ReadPublic.
	ccNVReadPublic common.TPM_CC = 0x00000169
)

// RHOwner is TPM_RH_OWNER (the storage/owner hierarchy permanent handle), the
// authHandle under which these NV operations are authorized. It mirrors
// common.RHOwner as a bare uint32 for this package's marshaling helpers. TCG
// "TPM 2.0 Part 2: Structures", clause "TPM_RH (Permanent Handles)".
const RHOwner uint32 = 0x40000001

// TPMA_NV attribute bits. TPMA_NV is a UINT32 bit field (encoded big-endian).
// TCG "TPM 2.0 Part 2: Structures", clause "TPMA_NV". The four bits below are
// the read/write controls an owner-authorized ordinary index sets: the index
// may be written/read either by presenting owner authorization (OWNERWRITE /
// OWNERREAD) or by presenting the index's own authValue (AUTHWRITE / AUTHREAD).
const (
	// NVOwnerWrite (bit 1): owner authorization may write the index.
	NVOwnerWrite uint32 = 1 << 1
	// NVAuthWrite (bit 2): the index's authValue may authorize a write.
	NVAuthWrite uint32 = 1 << 2
	// NVOwnerRead (bit 17): owner authorization may read the index.
	NVOwnerRead uint32 = 1 << 17
	// NVAuthRead (bit 18): the index's authValue may authorize a read.
	NVAuthRead uint32 = 1 << 18

	// nvOrdinaryAttributes is the TPMA_NV used by the validation index: an
	// ordinary (data) index readable and writable under either owner or index
	// authorization.
	//
	//	OWNERWRITE | AUTHWRITE | OWNERREAD | AUTHREAD
	//	= (1<<1)|(1<<2)|(1<<17)|(1<<18)
	//	= 0x02 | 0x04 | 0x20000 | 0x40000
	//	= 0x00060006
	nvOrdinaryAttributes = NVOwnerWrite | NVAuthWrite | NVOwnerRead | NVAuthRead
)

// NVPublic is the parsed public area of an NV index: the typed form of the
// TPMS_NV_PUBLIC wire structure. TCG "TPM 2.0 Part 2: Structures", clause
// "TPMS_NV_PUBLIC".
type NVPublic struct {
	// Index is the NV index handle (nvIndex), a handle in the 0x01xxxxxx
	// (TPM_HT_NV_INDEX) range.
	Index uint32
	// NameAlg is the index's name algorithm (a TPM_ALG_ID).
	NameAlg uint16
	// Attributes is the TPMA_NV attribute bit field.
	Attributes uint32
	// AuthPolicy is the index's authorization policy digest (empty for an
	// index with no policy).
	AuthPolicy []byte
	// DataSize is the size in bytes of the index's data area.
	DataSize uint16
}

// marshalNVPublic renders a TPMS_NV_PUBLIC, wrapped in the TPM2B_NV_PUBLIC
// size prefix. The inner TPMS_NV_PUBLIC is:
//
//	nvIndex    : TPMI_RH_NV_INDEX (u32)
//	nameAlg    : TPMI_ALG_HASH    (u16)
//	attributes : TPMA_NV          (u32)
//	authPolicy : TPM2B_DIGEST
//	dataSize   : UINT16
//
// TCG "TPM 2.0 Part 2: Structures", clauses "TPMS_NV_PUBLIC" and
// "TPM2B_NV_PUBLIC".
func marshalNVPublic(p NVPublic) []byte {
	var inner []byte
	inner = common.PutU32(inner, p.Index)
	inner = common.PutU16(inner, p.NameAlg)
	inner = common.PutU32(inner, p.Attributes)
	inner = append(inner, common.MarshalTPM2B(p.AuthPolicy)...)
	inner = common.PutU16(inner, p.DataSize)
	return common.MarshalTPM2B(inner)
}

// parseNVPublic decodes a TPMS_NV_PUBLIC from the front of b (the inner bytes
// of a TPM2B_NV_PUBLIC, already unwrapped). It returns the parsed public and
// the trailing bytes. TCG "TPM 2.0 Part 2: Structures", clause
// "TPMS_NV_PUBLIC".
func parseNVPublic(b []byte) (NVPublic, []byte, error) {
	index, ok := common.GetU32(b, 0)
	if !ok {
		return NVPublic{}, nil, common.ErrShortBuffer
	}
	nameAlg, ok := common.GetU16(b, 4)
	if !ok {
		return NVPublic{}, nil, common.ErrShortBuffer
	}
	attrs, ok := common.GetU32(b, 6)
	if !ok {
		return NVPublic{}, nil, common.ErrShortBuffer
	}
	policy, rest, err := common.UnmarshalTPM2B(b[10:])
	if err != nil {
		return NVPublic{}, nil, err
	}
	dataSize, ok := common.GetU16(rest, 0)
	if !ok {
		return NVPublic{}, nil, common.ErrShortBuffer
	}
	pc := make([]byte, len(policy))
	copy(pc, policy)
	return NVPublic{
		Index:      index,
		NameAlg:    nameAlg,
		Attributes: attrs,
		AuthPolicy: pc,
		DataSize:   dataSize,
	}, rest[2:], nil
}

// NVDefineSpace runs TPM2_NV_DefineSpace under TPM_RH_OWNER (empty-auth
// password session), defining an ordinary NV index. The new index gets an
// empty authValue (the TPM2B_AUTH inSensitive is empty) and the public area
// built from the supplied NVPublic.
//
// Body (TagSessions):
//
//	handle area: authHandle (u32) = TPM_RH_OWNER
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_AUTH auth (empty) || TPM2B_NV_PUBLIC publicInfo
//
// Wire: TagSessions, CC 0x0000012A. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_NV_DefineSpace".
func (tpm *TPM) NVDefineSpace(pub NVPublic) error {
	body := common.PutU32(nil, RHOwner) // authHandle

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = common.PutU16(body, 0) // TPM2B_AUTH auth: empty index authValue
	body = append(body, marshalNVPublic(pub)...)

	_, err := tpm.execute(common.TagSessions, ccNVDefineSpace, body)
	return err
}

// NVUndefineSpace runs TPM2_NV_UndefineSpace under TPM_RH_OWNER (empty-auth
// password session), deleting the index at nvIndex.
//
// Body (TagSessions):
//
//	handle area: authHandle (u32) = TPM_RH_OWNER || nvIndex (u32)
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	(no parameters)
//
// Wire: TagSessions, CC 0x00000122. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_NV_UndefineSpace".
func (tpm *TPM) NVUndefineSpace(nvIndex uint32) error {
	body := common.PutU32(nil, RHOwner) // authHandle
	body = common.PutU32(body, nvIndex) // nvIndex

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	_, err := tpm.execute(common.TagSessions, ccNVUndefineSpace, body)
	return err
}

// NVWrite runs TPM2_NV_Write under TPM_RH_OWNER (empty-auth password session),
// writing data into the index at nvIndex starting at offset. The authHandle is
// the OWNER hierarchy, which authorizes the write because the index carries
// OWNERWRITE.
//
// Body (TagSessions):
//
//	handle area: authHandle (u32) = TPM_RH_OWNER || nvIndex (u32)
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: TPM2B_MAX_NV_BUFFER data || UINT16 offset
//
// Wire: TagSessions, CC 0x00000137. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_NV_Write".
func (tpm *TPM) NVWrite(nvIndex uint32, data []byte, offset uint16) error {
	body := common.PutU32(nil, RHOwner) // authHandle
	body = common.PutU32(body, nvIndex) // nvIndex

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = append(body, common.MarshalTPM2B(data)...) // TPM2B_MAX_NV_BUFFER
	body = common.PutU16(body, offset)                // offset

	_, err := tpm.execute(common.TagSessions, ccNVWrite, body)
	return err
}

// NVRead runs TPM2_NV_Read under TPM_RH_OWNER (empty-auth password session),
// reading size bytes from the index at nvIndex starting at offset. The OWNER
// authorization is accepted because the index carries OWNERREAD.
//
// Body (TagSessions):
//
//	handle area: authHandle (u32) = TPM_RH_OWNER || nvIndex (u32)
//	auth   area: authorizationSize (u32) || TPMS_AUTH_COMMAND (RS_PW, empty)
//	param  area: UINT16 size || UINT16 offset
//
// Wire: TagSessions, CC 0x0000014E. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_NV_Read".
//
// Response (TagSessions): parameterSize (u32) || TPM2B_MAX_NV_BUFFER data.
func (tpm *TPM) NVRead(nvIndex uint32, size, offset uint16) ([]byte, error) {
	body := common.PutU32(nil, RHOwner) // authHandle
	body = common.PutU32(body, nvIndex) // nvIndex

	auth := marshalPasswordAuth()
	body = common.PutU32(body, uint32(len(auth)))
	body = append(body, auth...)

	body = common.PutU16(body, size)   // size
	body = common.PutU16(body, offset) // offset

	rp, err := tpm.execute(common.TagSessions, ccNVRead, body)
	if err != nil {
		return nil, err
	}
	// parameterSize (TagSessions response).
	if _, ok := common.GetU32(rp, 0); !ok {
		return nil, common.ErrShortBuffer
	}
	d, _, err := common.UnmarshalTPM2B(rp[4:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(d))
	copy(out, d)
	return out, nil
}

// NVReadPublic runs TPM2_NV_ReadPublic, reading back the public area and the
// computed Name of the index at nvIndex. It carries no authorization (the
// public area is world-readable).
//
// Body (TagNoSessions):
//
//	handle area: nvIndex (u32)
//	(no parameters)
//
// Wire: TagNoSessions, CC 0x00000169. TCG "TPM 2.0 Part 3: Commands", clause
// "TPM2_NV_ReadPublic".
//
// Response (TagNoSessions): TPM2B_NV_PUBLIC nvPublic || TPM2B_NAME nvName.
func (tpm *TPM) NVReadPublic(nvIndex uint32) (NVPublic, []byte, error) {
	body := common.PutU32(nil, nvIndex) // nvIndex

	rp, err := tpm.execute(common.TagNoSessions, ccNVReadPublic, body)
	if err != nil {
		return NVPublic{}, nil, err
	}
	// TPM2B_NV_PUBLIC: a size-prefixed TPMS_NV_PUBLIC.
	pubBytes, rest, err := common.UnmarshalTPM2B(rp)
	if err != nil {
		return NVPublic{}, nil, err
	}
	pub, _, err := parseNVPublic(pubBytes)
	if err != nil {
		return NVPublic{}, nil, err
	}
	// TPM2B_NAME nvName.
	name, _, err := common.UnmarshalTPM2B(rest)
	if err != nil {
		return NVPublic{}, nil, err
	}
	nameCopy := make([]byte, len(name))
	copy(nameCopy, name)
	return pub, nameCopy, nil
}
