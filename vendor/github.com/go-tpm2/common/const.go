// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/common authors. All rights reserved.

package common

// Constants below are drawn from TCG "TPM 2.0 Part 2: Structures"
// (the "Constants" / "Handles" sections) unless noted otherwise. Values
// are the canonical wire values and are encoded big-endian on the wire.

// TPM_ST is a structure tag. TCG "TPM 2.0 Part 2: Structures", clause
// "TPM_ST (Structure Tags)".
type TPM_ST uint16

const (
	// TagNoSessions tags a command/response whose body carries no
	// session area.
	TagNoSessions TPM_ST = 0x8001
	// TagSessions tags a command/response that carries one or more
	// authorization/audit sessions after the handle area.
	TagSessions TPM_ST = 0x8002
)

// TPM_CC is a command code. TCG "TPM 2.0 Part 2: Structures", clause
// "TPM_CC (Command Codes)". This is a starter set; the full table is
// large and grows with the command layer.
type TPM_CC uint32

const (
	CCStartup       TPM_CC = 0x00000144
	CCShutdown      TPM_CC = 0x00000145
	CCGetCapability TPM_CC = 0x0000017A
	CCGetRandom     TPM_CC = 0x0000017B
	CCPCRRead       TPM_CC = 0x0000017E
	CCPCRExtend     TPM_CC = 0x00000182
	CCQuote         TPM_CC = 0x00000158
	CCPCRReset      TPM_CC = 0x0000013D
	CCSelfTest      TPM_CC = 0x00000143
	CCCreate        TPM_CC = 0x00000153
	CCCreatePrimary TPM_CC = 0x00000131
	CCLoad          TPM_CC = 0x00000157
)

// TPM_RC is a response code. TCG "TPM 2.0 Part 2: Structures", clause
// "TPM_RC (Response Codes)".
type TPM_RC uint32

const (
	// RCSuccess indicates the command completed without error.
	RCSuccess TPM_RC = 0x000
)

// TPM_SU is a startup/shutdown type, the parameter to TPM2_Startup and
// TPM2_Shutdown. TCG "TPM 2.0 Part 2: Structures", clause
// "TPM_SU (Startup Type)".
type TPM_SU uint16

const (
	// SUClear requests a Startup(CLEAR) / Shutdown(CLEAR).
	SUClear TPM_SU = 0x0000
	// SUState requests a Startup(STATE) / Shutdown(STATE).
	SUState TPM_SU = 0x0001
)

// TPM_ALG is an algorithm identifier. TCG "TPM 2.0 Part 2: Structures",
// clause "TPM_ALG_ID", with the registered values published in the TCG
// "Algorithm Registry".
type TPM_ALG uint16

const (
	AlgSHA1   TPM_ALG = 0x0004
	AlgSHA256 TPM_ALG = 0x000B
	AlgSHA384 TPM_ALG = 0x000C
	AlgNull   TPM_ALG = 0x0010
)

// TPM_RH is a permanent (well-known) handle. TCG "TPM 2.0 Part 2:
// Structures", clause "TPM_RH (Permanent Handles)" and "TPM_HT (Handle
// Types)". PCR handles occupy the 0x00000000..0x0000001F range; this
// stack uses 0..23 (PCRFirst..PCRLast).
type TPM_RH uint32

const (
	// RHOwner is the Owner (Storage) hierarchy handle.
	RHOwner TPM_RH = 0x40000001
	// RHNull is the null hierarchy / null handle.
	RHNull TPM_RH = 0x40000007
	// RHPlatform is the Platform hierarchy handle.
	RHPlatform TPM_RH = 0x4000000C

	// PCRFirst is the handle of PCR[0]; PCR handles run 0..23.
	PCRFirst TPM_RH = 0x00000000
	// PCRLast is the handle of PCR[23], the last architecturally
	// defined PCR.
	PCRLast TPM_RH = 0x00000017
)
