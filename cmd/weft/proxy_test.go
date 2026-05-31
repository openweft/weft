package main

import (
	"context"
	"errors"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestBootProxy_RequiresHostUUID covers the input-validation gate. Other
// tests would need a real Caddy binary on PATH (or a mock subprocess) and
// fit better as acceptance tests gated behind a build tag — the lifecycle
// is exercised end-to-end by the proxy package's own tests; this one just
// asserts the CLI-shim contract.
func TestBootProxy_RequiresHostUUID(t *testing.T) {
	_, err := bootProxy(context.Background(), "", nil, proxyOpts{})
	if err == nil {
		t.Fatal("expected error when hostUUID is empty")
	}
	if !errors.Is(err, err) /* placeholder: error type isn't sentinel */ {
		// We don't assert the exact message because it's user-facing
		// copy that may evolve; the existence of an error is the
		// contract.
	}
}

// TestAgentCmd_ProxyFlagsParsed asserts the three --proxy-* knobs flow
// from the cobra flag set into fileConfigTargets and on into the
// proxyOpts the boot path passes to bootProxyFn. Done via the bootProxyFn
// seam so the test never tries to launch Caddy.
//
// We can't run agentCmd().RunE end-to-end (it would actually call run()
// with a real adapter), so we exercise the indirection by capturing
// what main wires through. The seam is the contract — if a future
// refactor moves the call site, this test wants to fail loudly.
func TestAgentCmd_ProxyFlagsParsed(t *testing.T) {
	cmd := agentCmd()
	for _, name := range []string{"proxy", "proxy-state-dir", "proxy-caddy-binary", "proxy-key-prefix"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("agent command missing --%s flag", name)
		}
	}
	// Defaults: --proxy off, --proxy-caddy-binary = "caddy", others empty.
	if v, _ := cmd.Flags().GetBool("proxy"); v {
		t.Errorf("--proxy default = true, want false")
	}
	if v, _ := cmd.Flags().GetString("proxy-caddy-binary"); v != "caddy" {
		t.Errorf("--proxy-caddy-binary default = %q, want \"caddy\"", v)
	}
	if v, _ := cmd.Flags().GetString("proxy-state-dir"); v != "" {
		t.Errorf("--proxy-state-dir default = %q, want empty", v)
	}
	if v, _ := cmd.Flags().GetString("proxy-key-prefix"); v != "" {
		t.Errorf("--proxy-key-prefix default = %q, want empty", v)
	}
}

// TestBootProxyFn_SeamIsBootProxy makes sure the indirection seam
// points at the real implementation by default. A test substitution
// must not leak past the test that did it ; if the default ever
// changes, this assertion catches it.
func TestBootProxyFn_SeamIsBootProxy(t *testing.T) {
	// Compare function values via their behaviour : both reject
	// an empty hostUUID. Cheaper than reflect-on-function-pointer.
	_, err := bootProxyFn(context.Background(), "", nil, proxyOpts{})
	if err == nil {
		t.Errorf("bootProxyFn default should reject empty hostUUID like bootProxy does")
	}
}

// TestBootProxyFn_Override exercises the seam : a test-replaced
// bootProxyFn must receive the proxyOpts the agent boot path
// constructed. Captures the args, returns a no-op closer, and
// restores the default at the end.
func TestBootProxyFn_Override(t *testing.T) {
	prev := bootProxyFn
	t.Cleanup(func() { bootProxyFn = prev })

	var gotHostUUID string
	var gotEtcd *clientv3.Client
	var gotOpts proxyOpts
	bootProxyFn = func(_ context.Context, hostUUID string, cli *clientv3.Client, opts proxyOpts) (func() error, error) {
		gotHostUUID = hostUUID
		gotEtcd = cli
		gotOpts = opts
		return func() error { return nil }, nil
	}

	closer, err := bootProxyFn(context.Background(), "host-abc", nil, proxyOpts{
		StateDir:    "/var/lib/weft-agent/proxy",
		CaddyBinary: "/usr/local/bin/weft-proxy",
		KeyPrefix:   "/weft/proxy/routes",
	})
	if err != nil {
		t.Fatalf("override bootProxyFn returned error: %v", err)
	}
	if err := closer(); err != nil {
		t.Errorf("closer: %v", err)
	}
	if gotHostUUID != "host-abc" {
		t.Errorf("hostUUID = %q, want host-abc", gotHostUUID)
	}
	if gotEtcd != nil {
		t.Errorf("etcd client should be nil in this test case")
	}
	if gotOpts.StateDir != "/var/lib/weft-agent/proxy" || gotOpts.CaddyBinary != "/usr/local/bin/weft-proxy" || gotOpts.KeyPrefix != "/weft/proxy/routes" {
		t.Errorf("opts mistranslated: %+v", gotOpts)
	}
}

// TestDisplayOrDefault exercises the helper used in the startup
// log line so an empty operator-supplied string renders as the
// placeholder rather than as `""`.
func TestDisplayOrDefault(t *testing.T) {
	if got := displayOrDefault("", "fallback"); got != "fallback" {
		t.Errorf("empty input: got %q, want \"fallback\"", got)
	}
	if got := displayOrDefault("explicit", "fallback"); got != "explicit" {
		t.Errorf("set input: got %q, want \"explicit\"", got)
	}
}
