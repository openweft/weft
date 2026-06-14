package agent

// attest_node_stub_test.go exercises the node-side handshake stub with a
// fake AttestNode + a fake AttestationClient. It proves the 4-RPC wire
// contract (payload marshal/unmarshal + AK-name routing) end-to-end
// WITHOUT a TPM, mirroring how the control-plane handlers are tested on
// the server side.

import (
	"context"
	"errors"
	"testing"

	"github.com/go-tpm2/attest"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// compile-time: the real *attest.Node satisfies the AttestNode slice the
// stub drives (catches an upstream attest API change).
var _ AttestNode = (*attest.Node)(nil)

// fakeAttestNode answers the node side without a TPM. RespondEnroll echoes
// a fixed activation secret ; RespondAdmission returns a fixed quote. The
// values are opaque to the test (the fake server below doesn't verify
// them) — the point is the RPC plumbing, not the crypto (the crypto is
// covered by the server-side handshake test).
type fakeAttestNode struct {
	akName    []byte
	enrollErr error
	admitErr  error
}

func (f *fakeAttestNode) EnrollRequest(_ []byte) attest.EnrollRequest {
	return attest.EnrollRequest{AKName: f.akName, EKPub: []byte("ek"), AKPub: []byte("ak")}
}
func (f *fakeAttestNode) AKName() []byte { return f.akName }
func (f *fakeAttestNode) RespondEnroll(_ attest.EnrollChallenge) (attest.EnrollProof, error) {
	if f.enrollErr != nil {
		return attest.EnrollProof{}, f.enrollErr
	}
	return attest.EnrollProof{ActivationSecret: []byte("secret")}, nil
}
func (f *fakeAttestNode) AdmissionRequest() attest.AdmissionRequest {
	return attest.AdmissionRequest{AKName: f.akName}
}
func (f *fakeAttestNode) RespondAdmission(_ attest.AdmissionChallenge) (attest.AdmissionResponse, error) {
	if f.admitErr != nil {
		return attest.AdmissionResponse{}, f.admitErr
	}
	return attest.AdmissionResponse{Quoted: []byte("q"), Signature: make([]byte, 64)}, nil
}

// fakeAttestClient is a scripted control plane : it echoes back valid
// (empty-but-well-framed) challenge payloads and a configurable Admit
// outcome.
type fakeAttestClient struct {
	completeOk bool
	granted    bool
	reason     string
	admitName  []byte
	failAt     string // RPC name to fail at ("" = none)
}

func (c *fakeAttestClient) Enroll(_ context.Context, _ *weftv1.AttestMsg, _ ...grpc.CallOption) (*weftv1.AttestMsg, error) {
	if c.failAt == "Enroll" {
		return nil, errors.New("boom")
	}
	return &weftv1.AttestMsg{Payload: (attest.EnrollChallenge{}).Marshal()}, nil
}
func (c *fakeAttestClient) CompleteEnroll(_ context.Context, _ *weftv1.AttestMsg, _ ...grpc.CallOption) (*weftv1.AttestResult, error) {
	if c.failAt == "CompleteEnroll" {
		return nil, errors.New("boom")
	}
	return &weftv1.AttestResult{Ok: c.completeOk, Reason: c.reason}, nil
}
func (c *fakeAttestClient) RequestAdmission(_ context.Context, _ *weftv1.AttestMsg, _ ...grpc.CallOption) (*weftv1.AttestMsg, error) {
	if c.failAt == "RequestAdmission" {
		return nil, errors.New("boom")
	}
	return &weftv1.AttestMsg{Payload: (attest.AdmissionChallenge{}).Marshal()}, nil
}
func (c *fakeAttestClient) Admit(_ context.Context, _ *weftv1.AttestMsg, _ ...grpc.CallOption) (*weftv1.AdmitResult, error) {
	if c.failAt == "Admit" {
		return nil, errors.New("boom")
	}
	return &weftv1.AdmitResult{Granted: c.granted, Reason: c.reason, AkName: c.admitName}, nil
}

// TestRunAttestationHandshake_HappyPath drives the stub to a granted
// admission and checks the returned AK Name.
func TestRunAttestationHandshake_HappyPath(t *testing.T) {
	ak := []byte("node-ak")
	node := &fakeAttestNode{akName: ak}
	client := &fakeAttestClient{completeOk: true, granted: true, admitName: ak}
	got, err := RunAttestationHandshake(context.Background(), node, client, nil)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if string(got) != string(ak) {
		t.Fatalf("returned AK = %q ; want %q", got, ak)
	}
}

// TestRunAttestationHandshake_EnrolmentRejected surfaces a CompleteEnroll
// ok=false as an error.
func TestRunAttestationHandshake_EnrolmentRejected(t *testing.T) {
	node := &fakeAttestNode{akName: []byte("ak")}
	client := &fakeAttestClient{completeOk: false, reason: "bad proof"}
	if _, err := RunAttestationHandshake(context.Background(), node, client, nil); err == nil {
		t.Fatal("expected enrolment-rejected error")
	}
}

// TestRunAttestationHandshake_AdmissionDenied surfaces granted=false.
func TestRunAttestationHandshake_AdmissionDenied(t *testing.T) {
	node := &fakeAttestNode{akName: []byte("ak")}
	client := &fakeAttestClient{completeOk: true, granted: false, reason: "stale nonce"}
	if _, err := RunAttestationHandshake(context.Background(), node, client, nil); err == nil {
		t.Fatal("expected admission-denied error")
	}
}

// TestRunAttestationHandshake_RPCErrors covers each transport-failure
// branch + each node-side Respond* failure.
func TestRunAttestationHandshake_RPCErrors(t *testing.T) {
	ak := []byte("ak")
	for _, stage := range []string{"Enroll", "CompleteEnroll", "RequestAdmission", "Admit"} {
		node := &fakeAttestNode{akName: ak}
		client := &fakeAttestClient{completeOk: true, granted: true, admitName: ak, failAt: stage}
		if _, err := RunAttestationHandshake(context.Background(), node, client, nil); err == nil {
			t.Errorf("stage %s: expected error", stage)
		}
	}
	// Node RespondEnroll failure.
	if _, err := RunAttestationHandshake(context.Background(),
		&fakeAttestNode{akName: ak, enrollErr: errors.New("tpm activate failed")},
		&fakeAttestClient{completeOk: true, granted: true, admitName: ak}, nil); err == nil {
		t.Error("expected RespondEnroll error")
	}
	// Node RespondAdmission failure.
	if _, err := RunAttestationHandshake(context.Background(),
		&fakeAttestNode{akName: ak, admitErr: errors.New("tpm quote failed")},
		&fakeAttestClient{completeOk: true, granted: true, admitName: ak}, nil); err == nil {
		t.Error("expected RespondAdmission error")
	}
}
