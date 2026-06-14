//go:build darwin

package agent

// attest_node_agent_test.go covers the agent.start() attestation HOOK :
//
//   - flag OFF (default) : the TPM/handshake path is never entered, no
//     attestation label is stamped, and RegisterHost sees byte-for-byte
//     the operator's original labels — today's bring-up is unchanged.
//   - flag ON : the handshake runs, and on a granted admission the admitted
//     AK Name is stamped onto reg.Labels[AttestLabelKey] before RegisterHost
//     (the key the control-plane gate consumes). Driven via the
//     runNodeAttestationFn seam so it exercises the real handshake logic
//     against an in-process verifier client WITHOUT a TPM.
//   - flag ON, handshake fails : Start fails (attestation is required when
//     the flag is on — no silent fall-through to an unattested register).

import (
	"context"
	"errors"
	"testing"
)

// withAttestRunner swaps runNodeAttestationFn for the duration of a test,
// restoring it on cleanup.
func withAttestRunner(t *testing.T, fn func(context.Context, string, AttestationClient) ([]byte, error)) {
	t.Helper()
	orig := runNodeAttestationFn
	runNodeAttestationFn = fn
	t.Cleanup(func() { runNodeAttestationFn = orig })
}

// TestStart_AttestOff_BringupUnchanged : with AttestTPM unset the agent
// never touches the attestation path. The runner is swapped for a sentinel
// that fails the test if called, and RegisterHost must see exactly the
// operator's labels with no AttestLabelKey added.
func TestStart_AttestOff_BringupUnchanged(t *testing.T) {
	withAttestRunner(t, func(context.Context, string, AttestationClient) ([]byte, error) {
		t.Fatal("attestation runner must NOT be called when AttestTPM is false")
		return nil, nil
	})

	cp := newRecordingCP()
	a, err := New(Options{
		StateDir:     t.TempDir(),
		ControlPlane: cp,
		LocalHandles: &DriverHandles{}, // skip the driver-plugin launch
		Labels:       map[string]string{"role": "worker"},
		// AttestTPM defaults to false.
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start (attest off): %v", err)
	}
	if len(cp.registered) != 1 {
		t.Fatalf("expected 1 RegisterHost, got %d", len(cp.registered))
	}
	labels := cp.registered[0].Labels
	if _, ok := labels[AttestLabelKey]; ok {
		t.Errorf("attest off: RegisterHost labels must NOT carry %q, got %v", AttestLabelKey, labels)
	}
	if labels["role"] != "worker" || len(labels) != 1 {
		t.Errorf("attest off: labels altered, got %v ; want {role:worker}", labels)
	}
}

// TestStart_AttestOn_StampsAKName : with AttestTPM set the runner returns a
// granted AK Name, which Start must stamp onto RegisterHost's labels under
// AttestLabelKey while preserving the operator's other labels and NOT
// mutating the caller's Options.Labels map.
func TestStart_AttestOn_StampsAKName(t *testing.T) {
	const akName = "node-ak-name-bytes"
	withAttestRunner(t, func(_ context.Context, device string, client AttestationClient) ([]byte, error) {
		if client == nil {
			t.Error("attest on: expected a non-nil AttestationClient passed through")
		}
		return []byte(akName), nil
	})

	cp := newRecordingCP()
	origLabels := map[string]string{"role": "worker"}
	a, err := New(Options{
		StateDir:     t.TempDir(),
		ControlPlane: cp,
		LocalHandles: &DriverHandles{},
		Labels:       origLabels,
		AttestTPM:    true,
		AttestClient: &fakeAttestClient{completeOk: true, granted: true, admitName: []byte(akName)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start (attest on): %v", err)
	}
	if len(cp.registered) != 1 {
		t.Fatalf("expected 1 RegisterHost, got %d", len(cp.registered))
	}
	labels := cp.registered[0].Labels
	if labels[AttestLabelKey] != akName {
		t.Errorf("attest on: labels[%q] = %q ; want %q", AttestLabelKey, labels[AttestLabelKey], akName)
	}
	if labels["role"] != "worker" {
		t.Errorf("attest on: operator label dropped, got %v", labels)
	}
	// Copy-on-write : the caller's map must be untouched.
	if _, ok := origLabels[AttestLabelKey]; ok {
		t.Errorf("attest on: Options.Labels was mutated, got %v", origLabels)
	}
}

// TestStart_AttestOn_HandshakeFailsBringup : a failed handshake aborts Start
// (no register) — attestation is required when the flag is on.
func TestStart_AttestOn_HandshakeFailsBringup(t *testing.T) {
	withAttestRunner(t, func(context.Context, string, AttestationClient) ([]byte, error) {
		return nil, errors.New("admission denied: stale nonce")
	})

	cp := newRecordingCP()
	a, err := New(Options{
		StateDir:     t.TempDir(),
		ControlPlane: cp,
		LocalHandles: &DriverHandles{},
		AttestTPM:    true,
		AttestClient: &fakeAttestClient{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("attest on: Start must fail when the handshake fails")
	}
	if len(cp.registered) != 0 {
		t.Errorf("attest on failure: RegisterHost must NOT be called, got %d calls", len(cp.registered))
	}
}

// TestStart_AttestOn_RealHandshakeChain wires the start() hook to the REAL
// RunAttestationHandshake (driving a fake attest.Node over a scripted
// AttestationClient), proving the start() → handshake → label-stamp chain
// runs the actual four-RPC code rather than a stub return. The swtpm test
// upgrades both ends to real TPM + real verifier crypto ; this one locks in
// that start() routes through RunAttestationHandshake and stamps its result.
func TestStart_AttestOn_RealHandshakeChain(t *testing.T) {
	ak := []byte("ak-from-fake-node")
	node := &fakeAttestNode{akName: ak}

	withAttestRunner(t, func(ctx context.Context, _ string, client AttestationClient) ([]byte, error) {
		return RunAttestationHandshake(ctx, node, client, nil)
	})

	cp := newRecordingCP()
	a, err := New(Options{
		StateDir:     t.TempDir(),
		ControlPlane: cp,
		LocalHandles: &DriverHandles{},
		AttestTPM:    true,
		AttestClient: &fakeAttestClient{completeOk: true, granted: true, admitName: ak},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start (real handshake): %v", err)
	}
	if got := cp.registered[0].Labels[AttestLabelKey]; got != string(ak) {
		t.Errorf("labels[%q] = %q ; want %q", AttestLabelKey, got, ak)
	}
}
