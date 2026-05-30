// proxy.go — helper that brings up the reverse-proxy subsystem on the
// local agent: launches a Caddy subprocess (via agent/proxy.Supervisor),
// streams Route updates from etcd (via agent/proxy.Watcher), and applies
// them to Caddy through its admin API.
//
// Not yet called from the all-in-one boot path. Operators who want the
// proxy plane today wire it explicitly:
//
//	cli, _ := clientv3.New(...)
//	closer, err := bootProxy(ctx, hostUUID, cli, proxyOpts{StateDir: "/var/lib/weft-agent/proxy"})
//	if err != nil { ... }
//	defer closer()
//
// The intended wire point is after agent.Start in cmd/weft/run.go (all-
// in-one mode) and cmd/weft/run_client.go (client mode where the etcd
// client comes via the control-plane RPC, not a local connection — that
// path will need an etcd-over-gRPC bridge, deferred to a follow-up).
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

	"github.com/openweft/weft/agent/proxy"
	clientv3 "go.etcd.io/etcd/client/v3"
)

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
