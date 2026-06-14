// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tpm2 authors. All rights reserved.

// Package tpm2 is the transport-agnostic, pure-Go TPM 2.0 command API. It
// sits one layer above github.com/go-tpm2/common: common owns the
// big-endian wire codec and the Transport contract, and this package turns
// that codec into typed TPM 2.0 commands.
//
// A TPM value wraps a common.Transport. Each command method marshals its
// parameters (with common.BuildCommand and the appropriate TPM_ST tag and
// TPM_CC code), hands the buffer to Transport.Send, parses the reply with
// common.ParseResponse, checks the response code, and unmarshals the
// response parameters. A non-success response code is surfaced as a typed
// *TPMError carrying both the raw rc and the command code it came from.
//
// All multi-byte fields are encoded big-endian, as the TPM 2.0 wire format
// mandates (TCG "TPM 2.0 Part 1: Architecture", "Data Marshaling").
//
// This covers the measured-boot milestones: the starter set (Startup,
// Shutdown, SelfTest, GetRandom, GetCapability, PCR_Read, PCR_Extend); the
// attestation flow (CreatePrimary, Quote, VerifyQuote); the PCR-sealing
// payoff (CreateStoragePrimary, Create, Load, StartAuthSession, PolicyPCR,
// PolicyGetDigest, Unseal, plus the offline PolicyPCRDigest computation and
// the SealToPCR/UnsealWithPCR helpers — see seal.go); NV storage
// (NVDefineSpace, NVWrite, NVRead, NVReadPublic, NVUndefineSpace — see nv.go);
// typed GetCapability decoders (GetPCRBanks, GetTPMProperties,
// GetManufacturer, GetAlgorithms, GetHandles — see capability.go); the
// Endorsement Key (CreateEK, the EK Credential Profile ECC-P256 L-2 template —
// see ek.go); and Credential Protection / credential activation — the
// identity step of remote attestation that binds an Attestation Key to a
// TPM's Endorsement Key. That last flow is the off-TPM verifier side
// MakeCredential (ECDH + KDFe + KDFa + AES-CFB + HMAC, see makecred.go and the
// KDFa/KDFe helpers in kdf.go), the object Name it commits to (ObjectName, see
// name.go), and the attesting side TPM2_ActivateCredential with its
// dual-session authorization plus TPM2_PolicySecret to satisfy the EK policy,
// tied together by ActivateAKWithEK (see activate.go). Each method cites the
// relevant clause of TCG "TPM 2.0 Part 3:
// Commands" (with structure shapes from "Part 2: Structures", the policy
// digest / authorization rules from "Part 1: Architecture", and the EK
// template from the TCG "EK Credential Profile for TPM Family 2.0").
//
// Conventions: pure Go, CGO_ENABLED=0, no architecture-specific assembly,
// BSD-3-Clause on every file, 100% statement coverage, and GOWORK=off (the
// module is not part of any workspace).
package tpm2
