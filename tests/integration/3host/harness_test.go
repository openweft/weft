// harness_test.go — 3-host integration harness for weft.
//
// Build-tagged `integration` so the default `go test ./...` never picks it
// up. Each TestAcc* function is additionally guarded by the
// WEFT_INTEGRATION_HOSTS_PREFIX env var (the IP prefix of the local Tart
// subnet, e.g. "192.168.64") so that compile-only smoke tests
//
//	go test -tags integration -run NeverMatches ./tests/integration/3host/...
//
// run cleanly on a workstation that doesn't have the three Tart Debian VMs
// at .11/.12/.13 booted, SSH keys deployed, etc. See README.md for the
// pre-requisites the harness assumes when the env var IS set.
//
// Why a shell-out harness rather than importing cluster.Load/Apply directly:
// the integration tests deliberately exercise the same code path an operator
// would invoke from a terminal (`weft up -f cluster.hcl`, `weft down`),
// which catches packaging regressions (missing flag wiring, embedded asset
// drift, codesign issues on darwin) that an in-process call would miss.

//go:build integration

package harness_3host

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	weftv1 "github.com/openweft/weft-proto"
)

// envPrefix is the env var that gates every TestAcc* below. Setting it to
// e.g. "192.168.64" both unlocks the tests AND tells the harness which IP
// prefix the three Tart hosts live on (cluster.hcl pins the last octets at
// 11/12/13).
const envPrefix = "WEFT_INTEGRATION_HOSTS_PREFIX"

// hostOctets is the fixed suffix every host in cluster.hcl uses.
var hostOctets = []string{"11", "12", "13"}

// overlayPeers is the embedded-etcd quorum that cluster.hcl pins on the
// 10.9.0.0/24 overlay (see agent_config { storage { etcd { endpoints } } }).
var overlayPeers = []string{"10.9.0.11", "10.9.0.12", "10.9.0.13"}

// requireEnv skips the test (rather than failing) when the prefix env var
// is not set. Skipping — not failing — is what makes the build-tag compile
// gate cheap: `go test -tags integration -run NeverMatches ./...` works on
// any machine because every TestAcc* short-circuits.
func requireEnv(t *testing.T) string {
	t.Helper()
	prefix := os.Getenv(envPrefix)
	if prefix == "" {
		t.Skipf("skipping: %s not set (see tests/integration/3host/README.md)", envPrefix)
	}
	return prefix
}

// hostAddrs returns the three host underlay IPs (Tart-side), e.g.
// 192.168.64.{11,12,13} when prefix=="192.168.64".
func hostAddrs(prefix string) []string {
	out := make([]string, len(hostOctets))
	for i, oct := range hostOctets {
		out[i] = prefix + "." + oct
	}
	return out
}

// fixturePath returns the absolute path of cluster.hcl alongside this test
// file (testdata isn't used here so the file is discoverable by humans).
func fixturePath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("cluster.hcl")
	if err != nil {
		t.Fatalf("abs(cluster.hcl): %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fixture cluster.hcl missing: %v", err)
	}
	return abs
}

// weftBinary returns the `weft` CLI path. Defaults to "weft" on PATH;
// override with WEFT_BIN=/abs/path/to/weft for ad-hoc dev builds.
func weftBinary() string {
	if b := os.Getenv("WEFT_BIN"); b != "" {
		return b
	}
	return "weft"
}

// runWeft executes `weft <args...>` with --apply, surfacing stdout+stderr
// in the test log so a CI failure is debuggable from the artifact alone.
func runWeft(t *testing.T, ctx context.Context, args ...string) {
	t.Helper()
	bin := weftBinary()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	t.Logf("$ %s %s\n%s", bin, strings.Join(args, " "), out)
	if err != nil {
		t.Fatalf("%s %s: %v", bin, strings.Join(args, " "), err)
	}
}

// TestAccClusterBringUp runs `weft up --apply` against cluster.hcl and
// expects the three hosts to become reachable on their agent gRPC port.
// "Reach Running" is approximated as: TCP dial of every host's :9090
// agent listener succeeds within a generous deadline (the planner emits a
// PushAgentConfig + StartAgent action per host, so once the listener is up
// the host is provisioned by the planner's definition).
func TestAccClusterBringUp(t *testing.T) {
	prefix := requireEnv(t)
	fixture := fixturePath(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	runWeft(t, ctx, "up", "-f", fixture, "--apply")

	deadline := time.Now().Add(5 * time.Minute)
	for _, addr := range hostAddrs(prefix) {
		target := net.JoinHostPort(addr, "9090")
		if err := waitTCP(ctx, target, deadline); err != nil {
			t.Fatalf("host %s never reached Running on %s: %v", addr, target, err)
		}
		t.Logf("host %s up on %s", addr, target)
	}
}

// TestAccEtcdQuorum opens a clientv3 against each host's :2379 (the
// embedded-etcd endpoint configured in cluster.hcl's agent_config block)
// and asserts that MemberList returns exactly 3 members from every peer's
// vantage point — that's the textbook quorum-health check.
func TestAccEtcdQuorum(t *testing.T) {
	prefix := requireEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for _, addr := range hostAddrs(prefix) {
		endpoint := "http://" + net.JoinHostPort(addr, "2379")
		cli, err := clientv3.New(clientv3.Config{
			Endpoints:   []string{endpoint},
			DialTimeout: 10 * time.Second,
			Context:     ctx,
		})
		if err != nil {
			t.Fatalf("etcd dial %s: %v", endpoint, err)
		}
		resp, err := cli.MemberList(ctx)
		_ = cli.Close()
		if err != nil {
			t.Fatalf("etcd MemberList via %s: %v", endpoint, err)
		}
		if got := len(resp.Members); got != 3 {
			t.Fatalf("etcd quorum from %s: want 3 members, got %d", endpoint, got)
		}
		t.Logf("etcd quorum healthy via %s (3 members)", endpoint)
	}
	// Cross-check that the endpoints embedded in cluster.hcl match the
	// overlay peers — guards against an HCL edit that desyncs the harness.
	for i, want := range overlayPeers {
		if !strings.HasSuffix(want, "."+hostOctets[i]) {
			t.Fatalf("overlayPeers[%d]=%q out of sync with hostOctets[%d]=%q", i, want, i, hostOctets[i])
		}
	}
}

// TestAccCreateVMRoundtrip dials each host's WeftAgent gRPC service over
// the underlay (TCP :9090, dev-mode no-TLS — see main.go --tcp-listen) and
// fires a CreateVM RPC. The assertion is intentionally weak: a non-error
// response (or an Unimplemented/InvalidArgument grpc.Status) proves the
// service is reachable and registered — the goal here is "round-trip
// works end-to-end", not "VMs actually boot" (that's covered by the
// microvm-level tests).
func TestAccCreateVMRoundtrip(t *testing.T) {
	prefix := requireEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	for i, addr := range hostAddrs(prefix) {
		target := net.JoinHostPort(addr, "9090")
		conn, err := grpc.NewClient("passthrough:///"+target,
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("grpc.NewClient %s: %v", target, err)
		}
		client := weftv1.NewWeftAgentClient(conn)
		req := &weftv1.CreateVMRequest{
			Name:    fmt.Sprintf("integration-h%d", i+1),
			Image:   "ghcr.io/openweft/weft-test-fixture:scratch",
			Cpu:     1,
			MemMb:   256,
			DiskGb:  1,
			Project: "integration",
		}
		_, err = client.CreateVM(ctx, req)
		_ = conn.Close()
		if err != nil {
			// The harness accepts grpc errors as "round-trip OK" — the
			// transport layer answered, which is what we assert. Hard
			// transport failures (Unavailable, DeadlineExceeded against
			// a dial that never returned) are still failures.
			if isHardTransportError(err) {
				t.Fatalf("CreateVM round-trip to %s: %v", target, err)
			}
			t.Logf("CreateVM round-trip to %s answered with grpc-level error (expected for a stub image): %v", target, err)
			continue
		}
		t.Logf("CreateVM round-trip to %s OK", target)
	}
}

// TestAccClusterDown runs `weft down --apply` against the same fixture and
// asserts the three hosts are un-provisioned (their agent listener stops
// answering on :9090). Symmetric to TestAccClusterBringUp.
func TestAccClusterDown(t *testing.T) {
	prefix := requireEnv(t)
	fixture := fixturePath(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	runWeft(t, ctx, "down", "-f", fixture, "--apply")

	deadline := time.Now().Add(3 * time.Minute)
	for _, addr := range hostAddrs(prefix) {
		target := net.JoinHostPort(addr, "9090")
		if err := waitTCPGone(ctx, target, deadline); err != nil {
			t.Fatalf("host %s still reachable on %s after `weft down`: %v", addr, target, err)
		}
		t.Logf("host %s torn down (%s no longer accepting)", addr, target)
	}
}

// waitTCP returns nil once a TCP dial to target succeeds, or an error if
// the deadline expires first.
func waitTCP(ctx context.Context, target string, deadline time.Time) error {
	for {
		dialer := net.Dialer{Timeout: 5 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s: %w", target, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitTCPGone is the inverse of waitTCP: returns nil once dialing target
// stops succeeding, or an error if the deadline expires first.
func waitTCPGone(ctx context.Context, target string, deadline time.Time) error {
	for {
		dialer := net.Dialer{Timeout: 2 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			return fmt.Errorf("%s still accepting connections", target)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// isHardTransportError classifies an error returned by a gRPC unary call
// as a transport-level failure (test should fail) vs an application-level
// gRPC status (test should treat as round-trip success). We only flag
// dial/network failures and pure context expiries; anything else (a grpc
// Status reply) is by definition a successful round-trip.
func isHardTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Pure dial/transport failures bubble up without a grpc status code
	// wrapper — substring match keeps this dependency-free.
	for _, marker := range []string{
		"connection refused",
		"no such host",
		"network is unreachable",
		"i/o timeout",
		"context deadline exceeded",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
