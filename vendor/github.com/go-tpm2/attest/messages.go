// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

// This file defines the protocol's wire messages and their Marshal/Unmarshal
// methods over the codec in codec.go. There are two phases:
//
//   - ENROLMENT (once per node): the node presents its EK and AK; the verifier
//     issues a MakeCredential challenge bound to the AK Name and the EK; the
//     node proves possession by ActivateCredential and returns the recovered
//     secret, which binds the AK to a trusted EK in the registry.
//
//   - ADMISSION (every join): the verifier issues a fresh nonce; the node
//     returns a TPM Quote over that nonce, signed by the now-bound AK, plus the
//     claimed PCR values; the verifier checks the signature, the nonce
//     (anti-replay), the pcrDigest, and the golden PCR policy.

// EnrollRequest is the node's opening enrolment message: its Endorsement Key
// public area, the EK certificate (carried opaquely; trust is decided by the
// EKRegistry), the AK public area, and the AK Name (the value MakeCredential
// must commit to). EKPub is the TPMT_PUBLIC of the EK; AKPub the TPMT_PUBLIC of
// the AK; AKName the bare (alg||digest) Name from tpm2.ObjectName.
type EnrollRequest struct {
	EKPub  []byte
	EKCert []byte
	AKPub  []byte
	AKName []byte
}

// Marshal encodes the EnrollRequest to a self-describing frame.
func (m EnrollRequest) Marshal() []byte {
	return frame(kindEnrollRequest).
		bytes(m.EKPub).
		bytes(m.EKCert).
		bytes(m.AKPub).
		bytes(m.AKName).
		out()
}

// Unmarshal decodes a frame into the EnrollRequest.
func (m *EnrollRequest) Unmarshal(buf []byte) error {
	d := open(buf, kindEnrollRequest)
	m.EKPub = d.bytes()
	m.EKCert = d.bytes()
	m.AKPub = d.bytes()
	m.AKName = d.bytes()
	return d.finish()
}

// EnrollChallenge is the verifier's MakeCredential response: the
// credentialBlob (TPM2B_ID_OBJECT) and secret (TPM2B_ENCRYPTED_SECRET) the
// node feeds to TPM2_ActivateCredential. Only a TPM holding the EK private key
// AND an AK with the committed Name can recover the embedded activation secret.
type EnrollChallenge struct {
	CredentialBlob []byte
	Secret         []byte
}

// Marshal encodes the EnrollChallenge to a frame.
func (m EnrollChallenge) Marshal() []byte {
	return frame(kindEnrollChallenge).
		bytes(m.CredentialBlob).
		bytes(m.Secret).
		out()
}

// Unmarshal decodes a frame into the EnrollChallenge.
func (m *EnrollChallenge) Unmarshal(buf []byte) error {
	d := open(buf, kindEnrollChallenge)
	m.CredentialBlob = d.bytes()
	m.Secret = d.bytes()
	return d.finish()
}

// EnrollProof is the node's proof of possession: the activation secret it
// recovered from ActivateCredential. The verifier constant-time-compares it to
// the secret it embedded; equality binds the AK to the trusted EK.
type EnrollProof struct {
	ActivationSecret []byte
}

// Marshal encodes the EnrollProof to a frame.
func (m EnrollProof) Marshal() []byte {
	return frame(kindEnrollProof).
		bytes(m.ActivationSecret).
		out()
}

// Unmarshal decodes a frame into the EnrollProof.
func (m *EnrollProof) Unmarshal(buf []byte) error {
	d := open(buf, kindEnrollProof)
	m.ActivationSecret = d.bytes()
	return d.finish()
}

// AdmissionRequest is the node's request to join the fleet, naming the AK it
// enrolled. The verifier looks the AK Name up in its registry and refuses
// unbound AKs.
type AdmissionRequest struct {
	AKName []byte
}

// Marshal encodes the AdmissionRequest to a frame.
func (m AdmissionRequest) Marshal() []byte {
	return frame(kindAdmissionRequest).
		bytes(m.AKName).
		out()
}

// Unmarshal decodes a frame into the AdmissionRequest.
func (m *AdmissionRequest) Unmarshal(buf []byte) error {
	d := open(buf, kindAdmissionRequest)
	m.AKName = d.bytes()
	return d.finish()
}

// AdmissionChallenge is the verifier's fresh 32-byte anti-replay nonce, which
// the node folds into the Quote's extraData (qualifyingData). The verifier
// later checks the quoted attest's extraData equals exactly this nonce.
type AdmissionChallenge struct {
	Nonce [32]byte
}

// Marshal encodes the AdmissionChallenge to a frame.
func (m AdmissionChallenge) Marshal() []byte {
	return frame(kindAdmissionChallenge).
		fixed32(m.Nonce).
		out()
}

// Unmarshal decodes a frame into the AdmissionChallenge.
func (m *AdmissionChallenge) Unmarshal(buf []byte) error {
	d := open(buf, kindAdmissionChallenge)
	m.Nonce = d.fixed32()
	return d.finish()
}

// AdmissionResponse carries the node's TPM Quote: the raw TPM2B_ATTEST data
// (Quoted, the signed bytes), the ECDSA signature over it (Signature, as a
// concatenated r||s of equal halves), the claimed PCR values (PCRs, by index),
// and an optional measured-boot event log (EventLog, reserved for the v0.2
// EventLogPolicy). The verifier reconstructs the (r,s) split from the signed
// curve (P-256 => 32-byte halves).
type AdmissionResponse struct {
	Quoted    []byte
	Signature []byte
	PCRs      map[int][]byte
	EventLog  []byte
}

// Marshal encodes the AdmissionResponse to a frame.
func (m AdmissionResponse) Marshal() []byte {
	return frame(kindAdmissionResponse).
		bytes(m.Quoted).
		bytes(m.Signature).
		pcrMap(m.PCRs).
		bytes(m.EventLog).
		out()
}

// Unmarshal decodes a frame into the AdmissionResponse.
func (m *AdmissionResponse) Unmarshal(buf []byte) error {
	d := open(buf, kindAdmissionResponse)
	m.Quoted = d.bytes()
	m.Signature = d.bytes()
	m.PCRs = d.pcrMap()
	m.EventLog = d.bytes()
	return d.finish()
}
