// proxy.go — helper that brings up the reverse-proxy subsystem on the
// local agent: launches a Caddy subprocess (via agent/proxy.Supervisor),
// streams Route updates from etcd (via agent/proxy.Watcher), and applies
// them to Caddy through its admin API.
//
// Wired into the all-in-one boot path (cmd/weft/main.go `run()`) behind
// the `--proxy` flag — off by default so operators that don't need L7
// ingress keep the single-process daemon. Tunables flow through three
// companion flags : `--proxy-state-dir`, `--proxy-caddy-binary`,
// `--proxy-key-prefix`. See docs/operations/proxy.md for the full
// operator-facing rundown.
//
// Quick usage :
//
//	weft agent --proxy --proxy-caddy-binary=/usr/local/bin/weft-proxy
//
// Client mode (`weft agent --client --control-plane=URL`) does NOT wire
// the proxy : the per-host runtime reaches etcd through the control-
// plane gRPC bridge, not a local *clientv3.Client. An etcd-over-gRPC
// shim is the cleanest fix and lands in a follow-up ; the flag is
// logged-and-ignored under --client today.
//
// Keeping this in cmd/weft/ rather than agent/ on purpose: the agent
// package stays etcd-agnostic (only ControlPlane is the seam), and the
// operator-facing CLI layer composes subsystems. Same shape as
// embed_etcd.go + storage_factory.go.

package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"syscall"

	"github.com/openweft/weft/agent/proxy"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// bootProxyFn is the indirection seam the agent boot path goes
// through to start the proxy plane. Tests substitute it to assert
// the flag/option translation without actually launching Caddy.
// Default = the real bootProxy below.
var bootProxyFn = bootProxy

// signalContext returns a context cancelled on SIGINT / SIGTERM.
// Kept here (rather than in main.go) because the proxy lifecycle
// is the first caller that genuinely needs a cancellable top-level
// ctx — pre-proxy run() did `select {}`.
func signalContext() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// displayOrDefault renders an operator-supplied string, falling
// back to a placeholder for the empty case so the startup log line
// stays readable.
func displayOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// proxyOpts bundles the operator-tunable knobs that flow into the
// proxy subsystem. Each field has a sane default so an empty proxyOpts
// is a working setup (filesystem cert storage, "caddy" on PATH, /var/lib
// state).
type proxyOpts struct {
	// StateDir is where the Caddy admin socket + cert storage land.
	// Empty → proxy.Options default ($XDG_RUNTIME_DIR/weft-agent-proxy).
	StateDir string
	// CaddyBinary names the caddy executable. Empty → "caddy" on PATH.
	// Operators who need the etcd-storage-etcd module compiled in
	// should point this at an xcaddy-built binary.
	CaddyBinary string
	// KeyPrefix overrides the default etcd key prefix the watcher
	// streams from. Empty → "/weft/proxy/routes" (matches the Watcher
	// default; left here so operators with multi-cluster shared etcd
	// can namespace differently).
	KeyPrefix string
}

// bootProxy starts the proxy.Supervisor + proxy.Watcher and returns a
// closer that tears both down in the right order (watcher cancelled
// first via ctx, then supervisor closed). Safe to call closer twice.
//
// Caller responsibilities:
//   - Pass an already-Start()ed clientv3.Client. The watcher reuses it
//     for the entire daemon lifetime; nil is allowed but yields a
//     "supervisor-only" mode (Caddy starts with empty routes and never
//     receives updates — useful for smoke-testing the Caddy lifecycle).
//   - hostUUID is the value the watcher uses as the etcd key suffix
//     (`<KeyPrefix>/<hostUUID>`). Mandatory.
//   - The returned closer is idempotent.
func bootProxy(ctx context.Context, hostUUID string, etcdCli *clientv3.Client, opts proxyOpts) (func() error, error) {
	if hostUUID == "" {
		return nil, fmt.Errorf("bootProxy: hostUUID is required")
	}

	sup := proxy.New(proxy.Options{
		StateDir:    opts.StateDir,
		CaddyBinary: opts.CaddyBinary,
	})
	if err := sup.Start(ctx); err != nil {
		return nil, fmt.Errorf("proxy: start caddy: %w", err)
	}

	// Watcher goroutine — only spun up when etcdCli is non-nil.
	// Cancellation propagates through wctx; we Close() the watcher's
	// context to short-circuit its long-poll on shutdown.
	wctx, wcancel := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	if etcdCli != nil {
		w := &proxy.Watcher{
			Client:     etcdCli,
			KeyPrefix:  opts.KeyPrefix,
			HostID:     hostUUID,
			Supervisor: sup,
		}
		go func() {
			defer close(watcherDone)
			if err := w.Run(wctx); err != nil && wctx.Err() == nil {
				// Only log when the watcher exited unexpectedly
				// (i.e. ctx wasn't already cancelled). An expected
				// shutdown returns ctx.Err() == context.Canceled
				// which we don't surface.
				log.Printf("weft proxy: watcher exited: %v", err)
			}
		}()
	} else {
		close(watcherDone)
	}

	var closed bool
	closer := func() error {
		if closed {
			return nil
		}
		closed = true
		wcancel()
		<-watcherDone
		return sup.Close()
	}
	return closer, nil
}
