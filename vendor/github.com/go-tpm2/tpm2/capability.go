// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

package tpm2

import "github.com/go-tpm2/common"

// This file adds TYPED decoders over the raw GetCapability already provided by
// commands.go. Each decoder selects one TPM_CAP, issues the raw command, and
// parses the capability-specific TPMU_CAPABILITIES union member the TPM
// returns. TCG "TPM 2.0 Part 3: Commands", clause "TPM2_GetCapability";
// "TPM 2.0 Part 2: Structures", clauses "TPMS_CAPABILITY_DATA",
// "TPMU_CAPABILITIES", and the per-capability list structures.

// TPM_CAP capability selectors. TCG "TPM 2.0 Part 2: Structures", clause
// "TPM_CAP". Encoded big-endian as UINT32.
const (
	// capAlgs is TPM_CAP_ALGS: the TPM's implemented algorithms, returned as a
	// TPML_ALG_PROPERTY.
	capAlgs uint32 = 0x00000000
	// capHandles is TPM_CAP_HANDLES: a list of in-use handles of the type the
	// property selects, returned as a TPML_HANDLE.
	capHandles uint32 = 0x00000001
	// capPCRs is TPM_CAP_PCRS: the PCR banks (allocation), returned as a
	// TPML_PCR_SELECTION.
	capPCRs uint32 = 0x00000005
	// capTPMProperties is TPM_CAP_TPM_PROPERTIES: fixed/variable properties,
	// returned as a TPML_TAGGED_TPM_PROPERTY.
	capTPMProperties uint32 = 0x00000006
)

// TPM_PT property tags within TPM_CAP_TPM_PROPERTIES. TCG "TPM 2.0 Part 2:
// Structures", clause "TPM_PT". PTFixed (0x100) is the base of the fixed
// (manufacturer/firmware) property group.
const (
	// PTFixed is PT_FIXED, the group base; it is also the property value to
	// start a fixed-property enumeration from.
	PTFixed uint32 = 0x00000100
	// PTManufacturer is TPM_PT_MANUFACTURER (PT_FIXED + 5): the four-character
	// vendor identifier (e.g. "IBM" for swtpm) packed big-endian into a UINT32.
	PTManufacturer uint32 = PTFixed + 5
)

// TaggedProperty is one entry of a TPML_TAGGED_TPM_PROPERTY: a property tag
// and its UINT32 value. TCG "TPM 2.0 Part 2: Structures", clause
// "TPMS_TAGGED_PROPERTY".
type TaggedProperty struct {
	// Property is the TPM_PT tag.
	Property uint32
	// Value is the property's UINT32 value.
	Value uint32
}

// AlgProperty is one entry of a TPML_ALG_PROPERTY: an algorithm id and its
// TPMA_ALGORITHM attribute bits. TCG "TPM 2.0 Part 2: Structures", clause
// "TPMS_ALG_PROPERTY".
type AlgProperty struct {
	// Alg is the TPM_ALG_ID.
	Alg uint16
	// Attributes is the TPMA_ALGORITHM bit field (asymmetric, symmetric,
	// hash, object, signing, encrypting, method).
	Attributes uint32
}

// getCapabilityData runs the raw GetCapability and checks the echoed
// capability selector before returning the union member bytes (everything
// after the capability:u32 in TPMS_CAPABILITY_DATA). It is the shared front
// end of the typed decoders.
func (tpm *TPM) getCapabilityData(capSel, prop, count uint32) ([]byte, error) {
	_, data, err := tpm.GetCapability(capSel, prop, count)
	if err != nil {
		return nil, err
	}
	// TPMS_CAPABILITY_DATA: capability (u32) || union member.
	echoed, ok := common.GetU32(data, 0)
	if !ok {
		return nil, common.ErrShortBuffer
	}
	if echoed != capSel {
		return nil, ErrUnexpectedCapability
	}
	return data[4:], nil
}

// maxPCRBanks is the propertyCount passed to TPM2_GetCapability(TPM_CAP_PCRS):
// the maximum number of PCR banks (TPMS_PCR_SELECTION entries) to return in
// one reply. The TPM 2.0 architecture defines at most a handful of allocated
// banks (SHA1, SHA256, SHA384, SHA512, SM3), so a small bound returns them
// all. A propertyCount of 0 makes some TPMs (including swtpm) return an EMPTY
// list with moreData set, which is why a non-zero count is required here. TCG
// "TPM 2.0 Part 3: Commands", clause "TPM2_GetCapability".
const maxPCRBanks uint32 = 16

// GetPCRBanks runs TPM2_GetCapability(TPM_CAP_PCRS) and decodes the resulting
// TPML_PCR_SELECTION: the TPM's allocated PCR banks (one TPMS_PCR_SELECTION per
// bank, the selection bitmap marking which PCRs the bank implements). TCG
// "TPM 2.0 Part 2: Structures", clauses "TPM_CAP_PCRS" / "TPML_PCR_SELECTION".
func (tpm *TPM) GetPCRBanks() ([]PCRSelection, error) {
	member, err := tpm.getCapabilityData(capPCRs, 0, maxPCRBanks)
	if err != nil {
		return nil, err
	}
	// TPM_CAP_PCRS union member is itself a TPML_PCR_SELECTION.
	sel, _, err := parsePCRSelectionList(member)
	if err != nil {
		return nil, err
	}
	return sel, nil
}

// GetTPMProperties runs TPM2_GetCapability(TPM_CAP_TPM_PROPERTIES) starting at
// firstProp and decodes the TPML_TAGGED_TPM_PROPERTY: a count followed by that
// many TPMS_TAGGED_PROPERTY { property:u32, value:u32 }. TCG "TPM 2.0 Part 2:
// Structures", clauses "TPM_CAP_TPM_PROPERTIES" / "TPML_TAGGED_TPM_PROPERTY".
func (tpm *TPM) GetTPMProperties(firstProp, count uint32) ([]TaggedProperty, error) {
	member, err := tpm.getCapabilityData(capTPMProperties, firstProp, count)
	if err != nil {
		return nil, err
	}
	n, ok := common.GetU32(member, 0)
	if !ok {
		return nil, common.ErrShortBuffer
	}
	off := 4
	var out []TaggedProperty
	for i := uint32(0); i < n; i++ {
		property, ok := common.GetU32(member, off)
		if !ok {
			return nil, common.ErrShortBuffer
		}
		value, ok := common.GetU32(member, off+4)
		if !ok {
			return nil, common.ErrShortBuffer
		}
		out = append(out, TaggedProperty{Property: property, Value: value})
		off += 8
	}
	return out, nil
}

// GetManufacturer is a convenience over GetTPMProperties: it reads
// TPM_PT_MANUFACTURER and returns its raw UINT32 value (the four packed ASCII
// vendor characters, big-endian). TCG "TPM 2.0 Part 2: Structures", clause
// "TPM_PT".
func (tpm *TPM) GetManufacturer() (uint32, error) {
	props, err := tpm.GetTPMProperties(PTManufacturer, 1)
	if err != nil {
		return 0, err
	}
	if len(props) == 0 || props[0].Property != PTManufacturer {
		return 0, ErrPropertyNotFound
	}
	return props[0].Value, nil
}

// GetAlgorithms runs TPM2_GetCapability(TPM_CAP_ALGS) starting at firstAlg and
// decodes the TPML_ALG_PROPERTY: a count followed by that many
// TPMS_ALG_PROPERTY { alg:u16, algProperties:TPMA_ALGORITHM(u32) }. TCG "TPM
// 2.0 Part 2: Structures", clauses "TPM_CAP_ALGS" / "TPML_ALG_PROPERTY".
func (tpm *TPM) GetAlgorithms(firstAlg, count uint32) ([]AlgProperty, error) {
	member, err := tpm.getCapabilityData(capAlgs, firstAlg, count)
	if err != nil {
		return nil, err
	}
	n, ok := common.GetU32(member, 0)
	if !ok {
		return nil, common.ErrShortBuffer
	}
	off := 4
	var out []AlgProperty
	for i := uint32(0); i < n; i++ {
		alg, ok := common.GetU16(member, off)
		if !ok {
			return nil, common.ErrShortBuffer
		}
		attrs, ok := common.GetU32(member, off+2)
		if !ok {
			return nil, common.ErrShortBuffer
		}
		out = append(out, AlgProperty{Alg: alg, Attributes: attrs})
		off += 6
	}
	return out, nil
}

// GetHandles runs TPM2_GetCapability(TPM_CAP_HANDLES) starting at firstHandle
// and decodes the TPML_HANDLE: a count followed by that many TPM_HANDLE
// (u32). The starting handle selects the handle range (e.g. 0x81000000 for
// persistent objects, 0x80000000 for transient). TCG "TPM 2.0 Part 2:
// Structures", clauses "TPM_CAP_HANDLES" / "TPML_HANDLE".
func (tpm *TPM) GetHandles(firstHandle, count uint32) ([]uint32, error) {
	member, err := tpm.getCapabilityData(capHandles, firstHandle, count)
	if err != nil {
		return nil, err
	}
	n, ok := common.GetU32(member, 0)
	if !ok {
		return nil, common.ErrShortBuffer
	}
	off := 4
	out := make([]uint32, 0, n)
	for i := uint32(0); i < n; i++ {
		h, ok := common.GetU32(member, off)
		if !ok {
			return nil, common.ErrShortBuffer
		}
		out = append(out, h)
		off += 4
	}
	return out, nil
}

// ErrUnexpectedCapability is returned when the TPM echoes a capability
// selector in TPMS_CAPABILITY_DATA other than the one requested.
const ErrUnexpectedCapability = common.Error("tpm2: unexpected capability in response")

// ErrPropertyNotFound is returned when a requested TPM_PT property is absent
// from the TPM's reply.
const ErrPropertyNotFound = common.Error("tpm2: requested TPM property not found")
