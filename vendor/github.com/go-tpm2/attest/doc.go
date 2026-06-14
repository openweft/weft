// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

// Package attest is a reusable TPM 2.0 remote-attestation protocol: a Verifier
// (control-plane) side and a Node (agent) side that together implement
// NODE-ADMISSION-ON-QUOTE. A node joins a fleet only if it proves, via a TPM
// Quote over a fresh verifier nonce signed by an EK-bound Attestation Key, that
// it booted an approved stack. It builds on github.com/go-tpm2/tpm2: the
// Verifier is pure Go and off-TPM (MakeCredential + VerifyQuote); the Node
// drives a real *tpm2.TPM (CreateEK/CreatePrimary/ActivateCredential/Quote).
//
// # Protocol
//
// The handshake has two phases. ENROLMENT runs once per node and binds an AK to
// a trusted EK; ADMISSION runs on every join and proves an approved boot.
//
// Enrolment (proves AK and a trusted EK live on the SAME TPM):
//
//	Node                                  Verifier
//	  -- EnrollRequest{EKPub,EKCert,         -->  Trusted(EKPub)? pick random
//	                   AKPub,AKName}              activation secret; MakeCredential
//	                                             (off-TPM) -> blob+secret
//	  <-- EnrollChallenge{CredentialBlob,    --
//	                      Secret}
//	  ActivateCredential(AK,EK,blob,secret)
//	  -> recovered secret
//	  -- EnrollProof{ActivationSecret}       -->  constant-time compare to pending;
//	                                             on match BindAK(AKName, AKPub)
//
// Admission (proves an approved boot, fresh each join):
//
//	Node                                  Verifier
//	  -- AdmissionRequest{AKName}            -->  AK bound? fresh Nonce; stash
//	  <-- AdmissionChallenge{Nonce}          --
//	  Quote(AK, pcrSel, Nonce) + PCRRead
//	  -- AdmissionResponse{Quoted,Signature, -->  VerifyQuote (sig + pcrDigest);
//	                       PCRs,EventLog}         extraData==Nonce (anti-replay);
//	                                             Policy.Matches(PCRs) -> granted
//
// # Security model
//
//   - EK enrolment / identity. MakeCredential binds an activation secret to the
//     AK's Name AND the EK's public key; only a TPM holding the EK private key
//     and an AK with that exact Name can recover it via ActivateCredential. A
//     successful CompleteEnroll therefore proves the AK and a TRUSTED EK live on
//     the same TPM. Which EKs are trusted is the EKRegistry's decision (an
//     allowlist, or an EK-certificate chain to a manufacturer root).
//
//   - Nonce anti-replay. Every admission uses a fresh verifier nonce, folded
//     into the quote's extraData (qualifyingData). Admit checks the signed
//     attest's extraData equals exactly the issued nonce and consumes the
//     pending nonce, so a captured quote cannot be replayed.
//
//   - Golden PCR policy. GoldenPolicy admits a node only if every required PCR
//     equals its expected ("golden") digest, naming the first mismatch in a
//     typed *UntrustedBootError (errors.Is ErrUntrustedBoot). The PCR values are
//     trusted only because the signature and the quoted pcrDigest were verified
//     first.
//
//   - Event-log replay policy (v0.2.0). EventLogPolicy admits a node by REPLAYING
//     its TCG measured-boot event log (the crypto-agile TCG_PCR_EVENT2 stream,
//     TCG PC Client Platform Firmware Profile) rather than matching whole-PCR
//     golden digests. Its Matches: (1) parses the log (ParseEventLog) and folds
//     each event's digest into a virtual PCR — pcr = SHA256(pcr || digest),
//     starting from all-zero (ReplayPCRs); (2) requires the replayed PCRs to
//     EQUAL the attested PCRs (ErrEventLogMismatch otherwise) — this is what
//     binds the otherwise-untrusted log to the cryptographically-verified quote,
//     so a tampered log whose replay diverges is rejected; (3) requires every
//     event to be on an allowlist of approved (PCRindex, digest) measurements
//     (ErrUnapprovedMeasurement, naming the offending event, otherwise).
//
//   - Allowlist vs. golden tradeoff. A GoldenPolicy entry is the FINAL PCR
//     value, i.e. the hash chain over every measurement that PCR accumulated;
//     changing any one component (a kernel, a shim, a config blob) changes the
//     final digest, so the operator must recompute and redeploy the golden PCR
//     for every platform/image permutation. An EventLogPolicy allowlist instead
//     approves INDIVIDUAL measurements: rolling out a new kernel image is ONE
//     new allowlist entry (its measurement), and any platform whose log is built
//     from approved components — in any valid order the replay reproduces — is
//     admitted. The cost is that the verifier now parses and replays an
//     attacker-influenced log; the replay-equality check (2) is the mitigation
//     that keeps the log honest. Use GoldenPolicy for a small, frozen fleet;
//     EventLogPolicy when images update independently and often.
//
//   - TOCTOU caveat. A Quote attests the platform's BOOT-TIME measurements (the
//     PCR state at quote time), NOT its runtime state. An approved boot says
//     nothing about post-boot compromise; attestation gates admission, it is not
//     a continuous runtime-integrity monitor. Pair it with short admission
//     leases / re-attestation for stronger guarantees.
//
// # tpm2 wiring
//
// The Node calls tpm2.CreateEK, tpm2.CreatePrimaryPublic, tpm2.ObjectName,
// tpm2.StartAuthSession, tpm2.PolicySecret, tpm2.ActivateCredential, tpm2.Quote
// and tpm2.PCRRead. The Verifier calls tpm2.MakeCredential and tpm2.VerifyQuote.
// VerifyQuote checks the ECDSA signature and the pcrDigest but does NOT compare
// extraData to a nonce, so Admit performs the nonce (anti-replay) check itself
// against the parsed AttestInfo.ExtraData.
package attest
