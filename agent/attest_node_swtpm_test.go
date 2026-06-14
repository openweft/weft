package agent

// attest_node_swtpm_test.go is the HIGH-FIDELITY node-side handshake test :
// it drives the REAL go-tpm2/attest *Node — backed by a real software TPM
// (swtpm) reached through go-tpm2/devtpm over swtpm's unix data socket,
// exactly as devtpm's own validation does — through RunAttestationHandshake
// against a REAL in-process attest.Verifier whose EK registry has the
// node's EK pre-enrolled via TrustEK.
//
// This is the closest off-hardware proof of the wiring : genuine CreateEK /
// CreatePrimary on the TPM, genuine ActivateCredential answering the
// verifier's MakeCredential challenge, genuine AK-signed Quote answering the
// admission nonce, and the genuine verifier crypto on the other end. The
// only thing it does NOT cover is a physical /dev/tpmrm0 (a separate
// hardware validation) — and runNodeAttestation's devtpm.Open path, which
// needs that device.
//
// It SKIPS (not fails) when swtpm is absent or won't start : the handshake
// LOGIC is independently covered off-TPM by attest_node_test.go, so a CI
// box without swtpm still proves the contract via the fakes ; this test
// upgrades that proof to a real TPM wherever swtpm exists.

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-tpm2/attest"
	"github.com/go-tpm2/devtpm"
	"github.com/go-tpm2/tpm2"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// verifierClient adapts a real *attest.Verifier to the AttestationClient
// interface the node handshake drives — the in-process stand-in for the
// gRPC AttestationService, calling the SAME verifier methods the control
// plane's handlers (cmd/weft/attestation.go) call. It mirrors that server
// side : marshal/unmarshal the opaque payloads, key on ak_name, mark the
// AK admitted on a granted decision.
type verifierClient struct {
	v        *attest.Verifier
	admitted map[string]bool
}

func newVerifierClient(v *attest.Verifier) *verifierClient {
	return &verifierClient{v: v, admitted: map[string]bool{}}
}

func (c *verifierClient) Enroll(_ context.Context, in *weftv1.AttestMsg, _ ...grpc.CallOption) (*weftv1.AttestMsg, error) {
	var er attest.EnrollRequest
	if err := er.Unmarshal(in.GetPayload()); err != nil {
		return nil, err
	}
	ch, err := c.v.Enroll(er)
	if err != nil {
		return nil, err
	}
	return &weftv1.AttestMsg{Payload: ch.Marshal()}, nil
}

func (c *verifierClient) CompleteEnroll(_ context.Context, in *weftv1.AttestMsg, _ ...grpc.CallOption) (*weftv1.AttestResult, error) {
	var proof attest.EnrollProof
	if err := proof.Unmarshal(in.GetPayload()); err != nil {
		return nil, err
	}
	if err := c.v.CompleteEnroll(in.GetAkName(), proof); err != nil {
		return &weftv1.AttestResult{Ok: false, Reason: err.Error()}, nil
	}
	return &weftv1.AttestResult{Ok: true}, nil
}

func (c *verifierClient) RequestAdmission(_ context.Context, in *weftv1.AttestMsg, _ ...grpc.CallOption) (*weftv1.AttestMsg, error) {
	var ar attest.AdmissionRequest
	if err := ar.Unmarshal(in.GetPayload()); err != nil {
		return nil, err
	}
	ch, err := c.v.Challenge(ar)
	if err != nil {
		return nil, err
	}
	return &weftv1.AttestMsg{Payload: ch.Marshal()}, nil
}

func (c *verifierClient) Admit(_ context.Context, in *weftv1.AttestMsg, _ ...grpc.CallOption) (*weftv1.AdmitResult, error) {
	var resp attest.AdmissionResponse
	if err := resp.Unmarshal(in.GetPayload()); err != nil {
		return nil, err
	}
	granted, err := c.v.Admit(in.GetAkName(), resp)
	if err != nil {
		return &weftv1.AdmitResult{Granted: false, Reason: err.Error(), AkName: in.GetAkName()}, nil
	}
	if granted {
		c.admitted[string(in.GetAkName())] = true
	}
	return &weftv1.AdmitResult{Granted: granted, AkName: in.GetAkName()}, nil
}

// compile-time : verifierClient satisfies the handshake's client interface.
var _ AttestationClient = (*verifierClient)(nil)

// startSwtpm launches swtpm in socket mode and returns a connected,
// already-started (--flags startup-clear) data channel as an
// io.ReadWriteCloser for devtpm.New, plus a cleanup func. It t.Skip()s when
// swtpm is unavailable or won't come up — keeping the suite green on boxes
// without a software TPM.
func startSwtpm(t *testing.T) (devConn net.Conn, cleanup func()) {
	t.Helper()
	bin, err := exec.LookPath("swtpm")
	if err != nil {
		t.Skipf("swtpm not installed: %v", err)
	}
	// A SHORT base dir : unix socket paths are capped at ~104 bytes
	// (sun_path) on darwin, and t.TempDir() under /var/folders/... blows
	// that. os.MkdirTemp("", ...) lands in $TMPDIR which is also long on
	// macOS, so anchor on /tmp explicitly and clean up ourselves.
	dir, err := os.MkdirTemp("/tmp", "weft-swtpm-")
	if err != nil {
		t.Skipf("mkdtemp for swtpm sockets: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dataSock := filepath.Join(dir, "swtpm.sock")
	ctrlSock := filepath.Join(dir, "swtpm-ctrl.sock")
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir swtpm state: %v", err)
	}

	cmd := exec.Command(bin, "socket",
		"--tpm2",
		"--server", "type=unixio,path="+dataSock,
		"--ctrl", "type=unixio,path="+ctrlSock,
		"--tpmstate", "dir="+stateDir,
		// not-need-init + startup-clear : swtpm powers on and issues
		// TPM2_Startup(CLEAR) itself, so the data channel is ready for
		// CreateEK without an explicit platform handshake from us.
		"--flags", "not-need-init,startup-clear",
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Skipf("swtpm start: %v", err)
	}

	// Wait for the data socket to accept a connection.
	var conn net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err = net.Dial("unix", dataSock)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Skipf("swtpm data socket never accepted: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cleanup = func() {
		_ = conn.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	return conn, cleanup
}

// TestRunAttestationHandshake_RealSwtpm drives the full four-RPC handshake
// with a real swtpm-backed *attest.Node against a real in-process
// *attest.Verifier (EK pre-enrolled via TrustEK), and asserts the admitted
// AK Name comes back matching the node's AKName — the value agent.start
// stamps onto reg.Labels[AttestLabelKey].
func TestRunAttestationHandshake_RealSwtpm(t *testing.T) {
	devConn, cleanup := startSwtpm(t)
	defer cleanup()

	// Real TPM transport over the swtpm data socket (devtpm.New is the
	// non-file path documented for exactly this — a unix socket honoring
	// the one-write/one-read framing).
	tr := devtpm.New(devConn)
	tpm := tpm2.New(tr)

	node, err := attest.NewNode(tpm, defaultPCRSelection())
	if err != nil {
		t.Fatalf("attest.NewNode on swtpm: %v", err)
	}

	// Control-plane side : a real verifier with the node's EK pre-trusted
	// (the operator's TrustEK step). The node's EK pub is what EnrollRequest
	// carries ; trust it so Enroll succeeds.
	reg := attest.NewMemRegistry()
	reg.TrustEK(node.EnrollRequest(nil).EKPub)
	v := attest.NewVerifier(reg, attest.GoldenPolicy{}, attest.RandNonce)

	// The node quotes PCRs 0–7. Pin the GoldenPolicy to the node's actual
	// current PCR values so Admit's policy check passes — the per-platform
	// golden-PCR step the verifier docs describe (SetPolicy from a measured
	// quote). measureGoldenPCRs reads them via a throwaway RespondAdmission ;
	// the real handshake produces its own fresh quote over the verifier's
	// nonce.
	pcrs := measureGoldenPCRs(t, node)
	v.SetPolicy(attest.GoldenPolicy(pcrs))

	client := newVerifierClient(v)

	got, err := RunAttestationHandshake(context.Background(), node, client, nil)
	if err != nil {
		t.Fatalf("RunAttestationHandshake against real swtpm + verifier: %v", err)
	}
	if string(got) != string(node.AKName()) {
		t.Fatalf("admitted AK Name = %x ; want node AKName %x", got, node.AKName())
	}
	if !client.admitted[string(node.AKName())] {
		t.Fatal("verifier did not record the AK as admitted")
	}
}

// measureGoldenPCRs reads the node's current PCR bank by having it answer a
// fixed admission challenge, returning the PCR map the verifier should pin
// as its GoldenPolicy. The returned response is otherwise discarded — the
// real handshake produces its own fresh quote over the verifier's nonce.
func measureGoldenPCRs(t *testing.T, node AttestNode) map[int][]byte {
	t.Helper()
	resp, err := node.RespondAdmission(attest.AdmissionChallenge{})
	if err != nil {
		t.Fatalf("measure golden PCRs (RespondAdmission): %v", err)
	}
	if len(resp.PCRs) == 0 {
		t.Fatal("node returned no PCRs to pin as golden policy")
	}
	return resp.PCRs
}
