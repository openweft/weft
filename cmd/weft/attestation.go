package main

// attestation.go is the server side of the AttestationService gRPC
// surface : the four RPCs (Enroll / CompleteEnroll / RequestAdmission /
// Admit) that drive the pure-Go go-tpm2/attest Verifier living on the
// weft control plane. Each handler unmarshals the opaque AttestMsg
// payload into the concrete attest wire type, calls the verifier method,
// and marshals the response — keeping weft-proto free of any TPM/attest
// dependency.
//
// These RPCs are deliberately NOT OIDC-gated : the cryptographic proof
// IS the authentication. They are inert (answer but gate nothing) unless
// the operator enabled attestation AND pre-trusted the node's EK.
//
// The gate itself — requiring a fresh admission before RegisterHost —
// lives in requireAdmittedAK below, which hosts.go's RegisterHost calls
// ONLY when s.attest.IsEnabled(). When the flag is off, RegisterHost
// never reaches this file (its OFF path is byte-for-byte unchanged).

import (
	"context"

	"github.com/go-tpm2/attest"
	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// attestUnavailable is returned when an AttestationService RPC arrives
// but the server has no attestation gate wired (the flag is off / nil).
// codes.Unavailable (not Unimplemented) because the service IS
// registered ; it is just not currently accepting attestation traffic.
func attestUnavailable(verb string) error {
	return status.Errorf(codes.Unavailable, "attestation is not enabled on this control plane (%s)", verb)
}

// Enroll begins enrolment. req.payload = attest.EnrollRequest.Marshal() ;
// resp.payload = attest.EnrollChallenge.Marshal(). The AK Name is inside
// the request payload, so req.ak_name is ignored here.
func (s *weftServer) Enroll(_ context.Context, req *weftv1.AttestMsg) (*weftv1.AttestMsg, error) {
	if !s.attest.IsEnabled() {
		return nil, attestUnavailable("enroll")
	}
	var er attest.EnrollRequest
	if err := er.Unmarshal(req.GetPayload()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode enroll request: %v", err)
	}
	ch, err := s.attest.Verifier.Enroll(er)
	if err != nil {
		// ErrUnknownEK and friends are caller-facing trust decisions, not
		// internal faults : surface as PermissionDenied so the node sees a
		// clean "your EK is not trusted" rather than a 500.
		return nil, status.Errorf(codes.PermissionDenied, "enroll: %v", err)
	}
	return &weftv1.AttestMsg{Payload: ch.Marshal()}, nil
}

// CompleteEnroll finishes enrolment. req.payload = attest.EnrollProof ;
// req.ak_name = the AK Name the node is enrolling (the verifier's
// pending-enrolment key). On a match the AK is bound to its trusted EK.
func (s *weftServer) CompleteEnroll(_ context.Context, req *weftv1.AttestMsg) (*weftv1.AttestResult, error) {
	if !s.attest.IsEnabled() {
		return nil, attestUnavailable("complete-enroll")
	}
	if len(req.GetAkName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ak_name is required")
	}
	var proof attest.EnrollProof
	if err := proof.Unmarshal(req.GetPayload()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode enroll proof: %v", err)
	}
	if err := s.attest.Verifier.CompleteEnroll(req.GetAkName(), proof); err != nil {
		// A failed activation is a benign "wrong proof" result, not a
		// transport fault : report it in-band so the node can retry the
		// whole handshake rather than treat it as a hard RPC error.
		return &weftv1.AttestResult{Ok: false, Reason: err.Error()}, nil
	}
	return &weftv1.AttestResult{Ok: true}, nil
}

// RequestAdmission opens an admission round. req.payload =
// attest.AdmissionRequest ; resp.payload = attest.AdmissionChallenge
// (a fresh anti-replay nonce). req.ak_name names the bound AK.
func (s *weftServer) RequestAdmission(_ context.Context, req *weftv1.AttestMsg) (*weftv1.AttestMsg, error) {
	if !s.attest.IsEnabled() {
		return nil, attestUnavailable("request-admission")
	}
	var ar attest.AdmissionRequest
	if err := ar.Unmarshal(req.GetPayload()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode admission request: %v", err)
	}
	ch, err := s.attest.Verifier.Challenge(ar)
	if err != nil {
		// ErrUnboundAK = the node never enrolled : a caller error.
		return nil, status.Errorf(codes.FailedPrecondition, "request admission: %v", err)
	}
	return &weftv1.AttestMsg{Payload: ch.Marshal()}, nil
}

// Admit decides admission. req.payload = attest.AdmissionResponse (quote
// + PCRs + signature) ; req.ak_name names the bound AK. On a granted
// decision the AK Name lands in the gate's short-TTL admitted set so the
// node's subsequent RegisterHost passes requireAdmittedAK.
func (s *weftServer) Admit(_ context.Context, req *weftv1.AttestMsg) (*weftv1.AdmitResult, error) {
	if !s.attest.IsEnabled() {
		return nil, attestUnavailable("admit")
	}
	if len(req.GetAkName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ak_name is required")
	}
	var resp attest.AdmissionResponse
	if err := resp.Unmarshal(req.GetPayload()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode admission response: %v", err)
	}
	granted, err := s.attest.Verifier.Admit(req.GetAkName(), resp)
	if err != nil {
		// A deny (bad quote / stale nonce / policy mismatch) is an in-band
		// result, not a transport fault : the node reads granted=false +
		// reason and decides whether to retry.
		return &weftv1.AdmitResult{Granted: false, Reason: err.Error(), AkName: req.GetAkName()}, nil
	}
	if granted {
		s.attest.MarkAdmitted(req.GetAkName())
	}
	return &weftv1.AdmitResult{Granted: granted, AkName: req.GetAkName()}, nil
}

// requireAdmittedAK is the RegisterHost gate, called ONLY on the attested
// path (s.attest.IsEnabled()). It consumes a fresh admission for akName :
// returns nil when the AK was admitted within the TTL (and drops the
// admission so it grants exactly one registration), else a gRPC
// PermissionDenied. akName is the hex-or-raw AK Name the node carries on
// its RegisterHostRequest.
func (s *weftServer) requireAdmittedAK(akName []byte) error {
	if len(akName) == 0 {
		return status.Error(codes.PermissionDenied, "attestation enabled: register host requires an admitted ak_name")
	}
	if !s.attest.ConsumeAdmission(akName) {
		return status.Error(codes.PermissionDenied, "attestation enabled: ak_name has no fresh admission — run the Enroll/Admit handshake first")
	}
	return nil
}

// attestGateEnabled is a tiny indirection so RegisterHost reads cleanly
// and tests can construct a weftServer with a nil gate (the OFF path).
func (s *weftServer) attestGateEnabled() bool { return s.attest.IsEnabled() }

// ensure the generated server interface stays satisfied.
var _ weftv1.AttestationServiceServer = (*weftServer)(nil)

// weftAttestGateFromConfig builds the AttestationGate at startup from the
// resolved config. enabled=false yields a disabled gate (the default) ;
// enabled=true wires the etcd-backed EK registry + the pure-Go verifier.
// kv is the KVStorage for the "/weft/attest/" prefix (etcd in HA, a
// file/mem KV in dev). Returns a usable, disabled gate on any wiring
// error so the daemon still boots with attestation simply off.
func weftAttestGateFromConfig(ctx context.Context, enabled bool, kv weft.KVStorage) (*weft.AttestationGate, error) {
	if !enabled {
		return weft.NewAttestationGate(false, nil, nil, 0), nil
	}
	reg, err := weft.LoadEKRegistry(ctx, kv)
	if err != nil {
		return nil, err
	}
	v := attest.NewVerifier(reg, attest.GoldenPolicy{}, attest.RandNonce)
	return weft.NewAttestationGate(true, reg, v, 0), nil
}
