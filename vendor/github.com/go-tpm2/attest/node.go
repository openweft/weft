// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

import (
	"github.com/go-tpm2/common"
	"github.com/go-tpm2/tpm2"
)

// RHEndorsement is TPM_RH_ENDORSEMENT, the hierarchy whose authValue the EK
// policy session asserts via PolicySecret during credential activation.
const rhEndorsement uint32 = 0x4000000B

// Node is the agent side of the protocol, running on the attesting platform. It
// drives a real *tpm2.TPM: it owns the EK and AK transient handles, their
// public areas, and the AK Name, and it answers the verifier's two challenges —
// RespondEnroll (ActivateCredential) and RespondAdmission (Quote + PCRRead).
//
// A Node is NOT safe for concurrent use: it serializes onto a single TPM, and
// credential activation opens a policy session per call.
type Node struct {
	tpm *tpm2.TPM

	ekHandle uint32
	ekPub    []byte // EK TPMT_PUBLIC
	akHandle uint32
	akPub    []byte // AK TPMT_PUBLIC
	akName   []byte // AK Name (alg || digest)

	// pcrSel is the PCR selection the node quotes and reads.
	pcrSel []tpm2.PCRSelection
}

// NewNode creates the EK and the AK on tpm and returns a ready Node. It runs
// CreateEK (the EK Credential Profile L-2 ECC P-256 key), CreatePrimaryPublic
// (an ECC P-256 restricted signing AK, returning its TPMT_PUBLIC), and derives
// the AK Name with ObjectName. pcrSel is the PCR selection the node will quote
// (e.g. SHA-256 bank, PCRs 0..7).
func NewNode(tpm *tpm2.TPM, pcrSel []tpm2.PCRSelection) (*Node, error) {
	ekHandle, ekPoint, err := tpm.CreateEK()
	if err != nil {
		return nil, err
	}
	// CreateEK returns only the parsed EK point (not the raw TPMT_PUBLIC). The
	// verifier needs only the point — for MakeCredential and trust matching —
	// so the node ships it inside a minimal, well-formed ECC TPMT_PUBLIC built
	// from the coordinates. (Were the verifier's EK trust keyed on the exact
	// EK public bytes, the node would instead transmit the TPM's verbatim
	// outPublic; see marshalECCPublicPoint.)
	ekPub := marshalECCPublicPoint(ekPoint.X, ekPoint.Y)

	akHandle, akPub, err := tpm.CreatePrimaryPublic()
	if err != nil {
		return nil, err
	}
	akName, err := tpm2.ObjectName(akPub)
	if err != nil {
		return nil, err
	}

	return &Node{
		tpm:      tpm,
		ekHandle: ekHandle,
		ekPub:    ekPub,
		akHandle: akHandle,
		akPub:    akPub,
		akName:   akName,
		pcrSel:   pcrSel,
	}, nil
}

// FromHandles builds a Node from pre-loaded EK and AK handles and their public
// areas (the contents of TPM2B_PUBLIC). akName is the AK Name; if nil it is
// derived from akPub via ObjectName. This is for callers that created or loaded
// the keys themselves. ekPub must be an ECC TPMT_PUBLIC carrying the EK point.
func FromHandles(tpm *tpm2.TPM, ekHandle uint32, ekPub []byte, akHandle uint32, akPub, akName []byte, pcrSel []tpm2.PCRSelection) (*Node, error) {
	if akName == nil {
		n, err := tpm2.ObjectName(akPub)
		if err != nil {
			return nil, err
		}
		akName = n
	}
	return &Node{
		tpm:      tpm,
		ekHandle: ekHandle,
		ekPub:    ekPub,
		akHandle: akHandle,
		akPub:    akPub,
		akName:   akName,
		pcrSel:   pcrSel,
	}, nil
}

// EnrollRequest returns the node's enrolment message: its EK public area, an
// (optional) EK certificate, its AK public area, and its AK Name. ekCert may be
// nil when the trust decision is made on the EK public alone.
func (n *Node) EnrollRequest(ekCert []byte) EnrollRequest {
	return EnrollRequest{
		EKPub:  n.ekPub,
		EKCert: ekCert,
		AKPub:  n.akPub,
		AKName: n.akName,
	}
}

// AKName returns the node's AK Name (used in AdmissionRequest and as the
// verifier's lookup key).
func (n *Node) AKName() []byte { return n.akName }

// AdmissionRequest returns the node's admission message naming its AK.
func (n *Node) AdmissionRequest() AdmissionRequest {
	return AdmissionRequest{AKName: n.akName}
}

// RespondEnroll answers the verifier's MakeCredential challenge by running
// TPM2_ActivateCredential: it opens a policy session, satisfies the EK policy
// with PolicySecret(TPM_RH_ENDORSEMENT), and activates the credential with the
// AK as activateHandle and the EK as keyHandle. The recovered activation secret
// (which only this TPM can produce) is returned as the proof.
func (n *Node) RespondEnroll(ch EnrollChallenge) (EnrollProof, error) {
	nonceCaller, err := n.tpm.GetRandom(nonceLen)
	if err != nil {
		return EnrollProof{}, err
	}
	session, _, err := n.tpm.StartAuthSession(nonceCaller)
	if err != nil {
		return EnrollProof{}, err
	}
	if err := n.tpm.PolicySecret(rhEndorsement, session); err != nil {
		return EnrollProof{}, err
	}
	secret, err := n.tpm.ActivateCredential(n.akHandle, n.ekHandle, session, ch.CredentialBlob, ch.Secret)
	if err != nil {
		return EnrollProof{}, err
	}
	return EnrollProof{ActivationSecret: secret}, nil
}

// RespondAdmission answers the verifier's nonce challenge with a TPM Quote over
// the node's PCR selection, folding the nonce into the quote's extraData, plus
// the current PCR values from PCRRead. The signature is rendered as the
// fixed-width r||s form the wire carries.
func (n *Node) RespondAdmission(ch AdmissionChallenge) (AdmissionResponse, error) {
	quoted, sig, err := n.tpm.Quote(n.akHandle, ch.Nonce[:], n.pcrSel)
	if err != nil {
		return AdmissionResponse{}, err
	}
	_, digests, err := n.tpm.PCRRead(n.pcrSel)
	if err != nil {
		return AdmissionResponse{}, err
	}

	pcrs := make(map[int][]byte)
	idx := 0
	for _, sel := range n.pcrSel {
		ordered := append([]int(nil), sel.PCRs...)
		sortInts(ordered)
		for _, pcr := range ordered {
			if idx >= len(digests) {
				return AdmissionResponse{}, ErrMalformedQuote
			}
			pcrs[pcr] = digests[idx]
			idx++
		}
	}

	return AdmissionResponse{
		Quoted:    quoted,
		Signature: JoinSignature(sig),
		PCRs:      pcrs,
	}, nil
}

// marshalECCPublicPoint builds a minimal ECC TPMT_PUBLIC carrying only the
// point (x, y) in its unique field — enough for the verifier's parseECCPoint
// and MakeCredential, which need only the coordinates. The template fields are
// the EK Credential Profile shape (ECC P-256, SHA-256 nameAlg) so the bytes are
// a well-formed TPMT_PUBLIC; the exact attributes are immaterial because the
// verifier only extracts the point. TCG "Part 2", "TPMT_PUBLIC".
//
// CreateEK returns only the parsed EKPublic point (not the raw public area), so
// the node reconstructs a public area here to ship the point in the same
// TPMT_PUBLIC shape parseECCPoint expects.
func marshalECCPublicPoint(x, y []byte) []byte {
	const (
		algECC      = 0x0023
		algSHA256   = 0x000B
		algAES      = 0x0006
		algCFB      = 0x0043
		eccNistP256 = 0x0003
	)
	var p []byte
	p = common.PutU16(p, algECC)    // type
	p = common.PutU16(p, algSHA256) // nameAlg
	p = common.PutU32(p, 0)         // objectAttributes (immaterial here)
	p = common.PutU16(p, 0)         // authPolicy: empty TPM2B
	// TPMS_ECC_PARMS: symmetric=AES-128-CFB (EK shape), scheme=NULL,
	// curve=P256, kdf=NULL.
	p = common.PutU16(p, algAES)
	p = common.PutU16(p, 128)
	p = common.PutU16(p, algCFB)
	p = common.PutU16(p, algNull)
	p = common.PutU16(p, eccNistP256)
	p = common.PutU16(p, algNull)
	// unique: TPMS_ECC_POINT { x, y }.
	p = append(p, common.MarshalTPM2B(x)...)
	p = append(p, common.MarshalTPM2B(y)...)
	return p
}
