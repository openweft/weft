package agent

// attest_node.go is the NODE-SIDE of the TPM remote-attestation handshake
// — the four-RPC dance a TPM-bearing weft node runs against the control
// plane's AttestationService before it is allowed to RegisterHost.
//
// WIRED : agent.start() calls runNodeAttestation BEFORE RegisterHost when
// Options.AttestTPM is set (the --attest-tpm flag). The default-OFF path
// never reaches this file — bring-up is byte-for-byte the legacy flow.
//
// Two layers live here :
//
//   - RunAttestationHandshake : transport-agnostic. It drives an
//     AttestNode (the *attest.Node interface) over an AttestationClient
//     (the generated gRPC client). No TPM, no gRPC concretion — so it is
//     unit-tested with fakes AND against a real attest.Verifier.
//   - runNodeAttestation : the production constructor. It opens the local
//     TPM via go-tpm2/devtpm, layers tpm2.New + attest.NewNode (which run
//     CreateEK / CreatePrimaryPublic on real TPM hardware), then calls
//     RunAttestationHandshake. This is the path the daemon takes ; it
//     needs a real /dev/tpmrm0 (or an swtpm socket), so it is exercised
//     on hardware as a separate validation, while the handshake logic
//     above is fully covered off-TPM.
//
// On a granted admission the caller (agent.start) stamps the returned AK
// Name onto reg.Labels[AttestLabelKey] — the key the control plane's
// flag-gated RegisterHost reads (cmd/weft/hosts.go attestAKLabel).
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
	"github.com/go-tpm2/common"
	"github.com/go-tpm2/devtpm"
	"github.com/go-tpm2/tpm2"
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
// It is driven from runNodeAttestation (the production TPM path) and is
// also exercised directly in tests with a fake AttestNode + a fake
// AttestationClient AND against a real attest.Verifier, keeping the wire
// contract verified end-to-end without a TPM.
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

// defaultPCRSelection is the PCR set the node quotes during admission : the
// SHA-256 bank, PCRs 0–7 (the platform firmware + boot-loader measurements
// a GoldenPolicy verifier pins). It mirrors the control plane's policy
// expectation ; a richer per-fleet selection is a follow-up (the verifier's
// GoldenPolicy is what ultimately decides which PCRs must match).
func defaultPCRSelection() []tpm2.PCRSelection {
	return []tpm2.PCRSelection{{
		Hash: uint16(common.AlgSHA256),
		PCRs: []int{0, 1, 2, 3, 4, 5, 6, 7},
	}}
}

// runNodeAttestationFn is the indirection agent.start() calls so tests can
// substitute the TPM-opening production path with an off-TPM handshake
// (driving a fake attest.Node against an in-process verifier client) and
// assert the AK-Name stamping without a TPM. Production points it at
// runNodeAttestation.
var runNodeAttestationFn = runNodeAttestation

// runNodeAttestation is the production TPM path : open the local TPM,
// derive an EK/AK, and run the four-RPC handshake against the control-plane
// AttestationService. It returns the admitted AK Name the agent stamps onto
// its RegisterHost labels. Any failure aborts bring-up — when the operator
// turns attestation on, a node that can't attest must not register.
//
// device empty defaults to devtpm.DefaultDevice ("/dev/tpmrm0"). client is
// the AttestationService gRPC client (built from the agent's control-plane
// conn) ; nil is a configuration error.
//
// REAL TPM REQUIRED : attest.NewNode runs CreateEK / CreatePrimaryPublic on
// the opened transport, so this needs a real /dev/tpmrm0 (bare-metal /
// vTPM) or an swtpm socket wrapped via devtpm.New — the separate hardware
// validation. The transport-agnostic RunAttestationHandshake it calls is
// what the unit tests cover off-TPM.
func runNodeAttestation(ctx context.Context, device string, client AttestationClient) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("attestation enabled but no AttestationService client wired (the attested path requires the gRPC control plane, not the in-process adapter)")
	}
	if device == "" {
		device = devtpm.DefaultDevice
	}
	tr, err := devtpm.Open(device)
	if err != nil {
		return nil, fmt.Errorf("open TPM %s: %w", device, err)
	}
	defer tr.Close()

	tpm := tpm2.New(tr)
	node, err := attest.NewNode(tpm, defaultPCRSelection())
	if err != nil {
		return nil, fmt.Errorf("derive EK/AK from TPM: %w", err)
	}
	// ekCert nil : the verifier's trust decision is on the EK public alone
	// (the operator pre-trusts the node's EK pub via the EK registry's
	// TrustEK before the node can Enroll). A platform EK certificate chain
	// is a follow-up trust mode.
	return RunAttestationHandshake(ctx, node, client, nil)
}
