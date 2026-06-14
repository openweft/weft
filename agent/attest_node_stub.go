package agent

// attest_node_stub.go is the NODE-SIDE sketch of the TPM
// remote-attestation handshake — the four-RPC dance a TPM-bearing weft
// node WOULD run against the control plane's AttestationService before it
// is allowed to RegisterHost.
//
// ┌──────────────────────────────────────────────────────────────────┐
// │  NOT WIRED — this is a documented stub, NOT a live code path.      │
// │                                                                    │
// │  No weft node has a TPM today (the openweft fleet is Apple-VZ /    │
// │  QEMU guests without a vTPM passed through). The function below    │
// │  compiles and type-checks against the published go-tpm2/attest     │
// │  Node API + the generated AttestationServiceClient, so it stays    │
// │  honest and rot-free — but it is never invoked from the daemon.    │
// │  RunAttestationHandshake REQUIRES a constructed *attest.Node,      │
// │  which in turn REQUIRES a real *tpm2.TPM transport (attest.NewNode │
// │  runs CreateEK / CreatePrimaryPublic on actual TPM hardware). When │
// │  a TPM-bearing node lands (e.g. a bare-metal host, or a guest with │
// │  a virtio/CRB vTPM via go-tpm2/crb|tis), the agent's bring-up      │
// │  calls this BEFORE NewGRPCControlPlane.RegisterHost, then passes   │
// │  the returned AK Name on the RegisterHost labels under             │
// │  weft.attest/ak-name (the key cmd/weft/hosts.go reads).            │
// └──────────────────────────────────────────────────────────────────┘
//
// Handshake (matches the control-plane AttestationService handlers in
// cmd/weft/attestation.go) :
//
//	node (this file)                         control plane
//	────────────────                         ─────────────
//	Node.EnrollRequest()  ──Enroll(req)────▶ Verifier.Enroll
//	Node.RespondEnroll(ch) ◀──challenge────  (MakeCredential)
//	         │            ──CompleteEnroll──▶ Verifier.CompleteEnroll
//	         │              (proof)           (binds AK→EK)
//	Node.AdmissionRequest()─RequestAdmission▶ Verifier.Challenge
//	Node.RespondAdmission ◀──nonce─────────   (fresh nonce)
//	         │            ──Admit(quote)────▶ Verifier.Admit
//	         ▼              (granted)          (→ admitted set, TTL)
//	RegisterHost(labels[weft.attest/ak-name]=akName)  ── gated by the
//	                                                     admitted set
//
// See cmd/weft/attestation.go for the server side and
// pkg/.../attestation.go (the weft.AttestationGate) for the gate.

import (
	"context"
	"fmt"

	"github.com/go-tpm2/attest"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// AttestationClient is the slim slice of the generated
// weftv1.AttestationServiceClient the node handshake needs. Defining the
// interface here keeps the stub testable (mock four methods) and mirrors
// the HostRegistryClient pattern in grpc_cp.go. The real generated client
// satisfies it structurally.
type AttestationClient interface {
	Enroll(ctx context.Context, in *weftv1.AttestMsg, opts ...grpc.CallOption) (*weftv1.AttestMsg, error)
	CompleteEnroll(ctx context.Context, in *weftv1.AttestMsg, opts ...grpc.CallOption) (*weftv1.AttestResult, error)
	RequestAdmission(ctx context.Context, in *weftv1.AttestMsg, opts ...grpc.CallOption) (*weftv1.AttestMsg, error)
	Admit(ctx context.Context, in *weftv1.AttestMsg, opts ...grpc.CallOption) (*weftv1.AdmitResult, error)
}

// AttestNode is the slim slice of the go-tpm2/attest *Node the handshake
// drives. *attest.Node satisfies it. Abstracting it keeps the stub
// compilable + testable without a TPM (a fake Node answers the two
// Respond* calls), while the production constructor (attest.NewNode)
// still needs real TPM hardware.
type AttestNode interface {
	EnrollRequest(ekCert []byte) attest.EnrollRequest
	AKName() []byte
	RespondEnroll(ch attest.EnrollChallenge) (attest.EnrollProof, error)
	AdmissionRequest() attest.AdmissionRequest
	RespondAdmission(ch attest.AdmissionChallenge) (attest.AdmissionResponse, error)
}

// RunAttestationHandshake drives the full Enroll→CompleteEnroll→
// RequestAdmission→Admit dance and returns the node's admitted AK Name on
// success. The caller stamps that AK Name onto its RegisterHost request
// (labels[weft.attest/ak-name] = string(akName)) so the control plane's
// flag-gated RegisterHost accepts the node.
//
// STUB STATUS : never called from the daemon today (no TPM-bearing node).
// It is exercised in tests with a fake AttestNode + a fake
// AttestationClient to keep the wire contract verified end-to-end. When a
// TPM lands, wire it into the agent bring-up (agent.go) ahead of
// ControlPlane.RegisterHost, guarded by an --attestation-enabled-equivalent
// node flag.
func RunAttestationHandshake(ctx context.Context, node AttestNode, client AttestationClient, ekCert []byte) ([]byte, error) {
	akName := node.AKName()

	// 1. Enroll : present EK + AK, receive a MakeCredential challenge.
	enrollReq := node.EnrollRequest(ekCert)
	enrollResp, err := client.Enroll(ctx, &weftv1.AttestMsg{Payload: enrollReq.Marshal()})
	if err != nil {
		return nil, fmt.Errorf("attest enroll: %w", err)
	}
	var challenge attest.EnrollChallenge
	if err := challenge.Unmarshal(enrollResp.GetPayload()); err != nil {
		return nil, fmt.Errorf("attest decode enroll challenge: %w", err)
	}

	// 2. CompleteEnroll : ActivateCredential on the TPM, return the proof.
	proof, err := node.RespondEnroll(challenge)
	if err != nil {
		return nil, fmt.Errorf("attest respond enroll (ActivateCredential): %w", err)
	}
	res, err := client.CompleteEnroll(ctx, &weftv1.AttestMsg{Payload: proof.Marshal(), AkName: akName})
	if err != nil {
		return nil, fmt.Errorf("attest complete enroll: %w", err)
	}
	if !res.GetOk() {
		return nil, fmt.Errorf("attest enrolment rejected: %s", res.GetReason())
	}

	// 3. RequestAdmission : receive a fresh anti-replay nonce.
	admReq := node.AdmissionRequest()
	chResp, err := client.RequestAdmission(ctx, &weftv1.AttestMsg{Payload: admReq.Marshal(), AkName: akName})
	if err != nil {
		return nil, fmt.Errorf("attest request admission: %w", err)
	}
	var admCh attest.AdmissionChallenge
	if err := admCh.Unmarshal(chResp.GetPayload()); err != nil {
		return nil, fmt.Errorf("attest decode admission challenge: %w", err)
	}

	// 4. Admit : Quote over the nonce, signed by the AK ; control plane
	//    verifies + records the admission.
	admResp, err := node.RespondAdmission(admCh)
	if err != nil {
		return nil, fmt.Errorf("attest respond admission (Quote): %w", err)
	}
	admit, err := client.Admit(ctx, &weftv1.AttestMsg{Payload: admResp.Marshal(), AkName: akName})
	if err != nil {
		return nil, fmt.Errorf("attest admit: %w", err)
	}
	if !admit.GetGranted() {
		return nil, fmt.Errorf("attest admission denied: %s", admit.GetReason())
	}
	return admit.GetAkName(), nil
}

// AttestLabelKey is the RegisterHostRequest.labels key the node sets to
// the admitted AK Name so the control-plane's flag-gated RegisterHost
// accepts it. MUST match attestAKLabel in cmd/weft/hosts.go.
const AttestLabelKey = "weft.attest/ak-name"

// compile-time assertion that the generated client satisfies the slim
// interface (catches a proto regeneration that changes the signatures).
var _ AttestationClient = (weftv1.AttestationServiceClient)(nil)
