package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Supervisor owns one Caddy subprocess and the channel used to push new
// route tables into it.
//
// Lifecycle:
//
//	s := proxy.New(proxy.Options{StateDir: "/var/lib/weft-agent/proxy"})
//	if err := s.Start(ctx); err != nil { ... }
//	defer s.Close()
//	s.Apply(routes)   // any time the route table changes
//
// Apply is safe to call concurrently with itself; the supervisor
// serialises POSTs to Caddy's admin endpoint via a mutex.
type Supervisor struct {
	opts Options

	cmd          *exec.Cmd
	adminSocket  string // unix socket path for the admin endpoint
	httpClient   *http.Client
	startOnce    sync.Once
	startErr     error
	mu           sync.Mutex
	currentBytes []byte // last successfully applied config — short-circuits no-op reloads
}

// Options bundles Supervisor inputs. None are strictly required; sane
// defaults apply when fields are zero.
type Options struct {
	// StateDir is where the admin socket + caddy data dir + log file go.
	// Defaults to $XDG_RUNTIME_DIR/weft-agent-proxy when zero.
	StateDir string

	// CaddyBinary names the caddy executable on $PATH. Defaults to
	// "caddy" — operators with a pinned (pkgx-managed) path can
	// override.
	CaddyBinary string

	// LogWriter is where caddy's stdout/stderr go. Defaults to
	// os.Stderr so an operator running `weft agent` sees Caddy logs
	// interleaved with the agent's own.
	LogWriter io.Writer
}

// New builds a Supervisor; doesn't start anything until Start is called.
func New(opts Options) *Supervisor {
	if opts.StateDir == "" {
		opts.StateDir = filepath.Join(defaultRuntimeDir(), "weft-agent-proxy")
	}
	if opts.CaddyBinary == "" {
		opts.CaddyBinary = "caddy"
	}
	if opts.LogWriter == nil {
		opts.LogWriter = os.Stderr
	}
	return &Supervisor{opts: opts}
}

// Start launches the Caddy subprocess with a minimal bootstrap config
// (admin socket only, no routes yet). Subsequent Apply calls push the
// real route table via the admin API.
func (s *Supervisor) Start(ctx context.Context) error {
	s.startOnce.Do(func() { s.startErr = s.startOnce_(ctx) })
	return s.startErr
}

func (s *Supervisor) startOnce_(ctx context.Context) error {
	if err := os.MkdirAll(s.opts.StateDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.opts.StateDir, err)
	}
	s.adminSocket = filepath.Join(s.opts.StateDir, "caddy-admin.sock")
	// Stale socket from a previous run prevents bind; remove before
	// starting. Idempotent — ENOENT is fine.
	_ = os.Remove(s.adminSocket)

	bootstrap := Routes(nil) // empty route table — Caddy starts idle
	// Operator opt-in: WEFT_PROXY_STORAGE_ETCD_ENDPOINTS=ep1,ep2,ep3 selects
	// the etcd-backed cert store (requires a caddy binary built with
	// caddy-storage-etcd via xcaddy). Unset → filesystem default under
	// StateDir/data.
	var storage map[string]any
	if eps := EtcdStorageEndpoints(); len(eps) > 0 {
		storage = storageEtcdConfig(eps)
		log.Printf("weft-agent proxy: shared cert storage = etcd (%d endpoint(s))", len(eps))
	}
	bootstrapBytes, err := bootstrap.renderCaddyConfigWith("unix//"+s.adminSocket, storage)
	if err != nil {
		return fmt.Errorf("render bootstrap config: %w", err)
	}

	s.cmd = exec.CommandContext(ctx, s.opts.CaddyBinary, "run", "--config", "-")
	s.cmd.Stdin = bytes.NewReader(bootstrapBytes)
	s.cmd.Stdout = s.opts.LogWriter
	s.cmd.Stderr = s.opts.LogWriter
	// Isolate Caddy's $XDG_DATA_HOME so cert storage doesn't collide
	// with an operator's interactive `caddy run` on the same machine.
	//
	// TODO(proxy-etcd-storage): swap filesystem cert storage for the
	// caddy-storage-etcd adapter so multiple agents share issued certs.
	// Today each host re-mints its own — fine for one-host clusters,
	// a coordination tax at 3-DC scale. See doc.go for context.
	s.cmd.Env = append(os.Environ(),
		"XDG_DATA_HOME="+filepath.Join(s.opts.StateDir, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(s.opts.StateDir, "cfg"),
	)
	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("start caddy: %w (is `%s` on PATH?)", err, s.opts.CaddyBinary)
	}
	log.Printf("weft-agent proxy: caddy started (pid=%d, admin=unix//%s)", s.cmd.Process.Pid, s.adminSocket)

	// Build an HTTP client that dials the admin socket. We use a fresh
	// http.Transport rather than the default so socket failures don't
	// poison http.DefaultClient.
	dialer := &net.Dialer{}
	s.httpClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", s.adminSocket)
			},
		},
	}

	// Wait until the admin socket actually exists + responds.
	// Caddy publishes the socket asynchronously; without a probe loop
	// the first Apply races and 502s.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.waitAdminReady(probeCtx); err != nil {
		_ = s.cmd.Process.Kill()
		return fmt.Errorf("admin endpoint did not come up: %w", err)
	}
	s.currentBytes = bootstrapBytes
	return nil
}

// waitAdminReady polls the admin /config/ endpoint until it returns 2xx
// or the ctx deadline fires.
func (s *Supervisor) waitAdminReady(ctx context.Context) error {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://caddy/config/", nil)
		resp, err := s.httpClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Apply pushes a new route table into the running Caddy. Idempotent: if
// the rendered config is byte-identical to the last applied config the
// call is a no-op (saves a Caddy reload and the brief connection drop
// it costs).
func (s *Supervisor) Apply(ctx context.Context, routes Routes) error {
	if s.cmd == nil {
		return errors.New("proxy: Start hasn't run yet")
	}
	// Storage block must survive every reload — POST /load replaces the
	// whole config, so dropping it here would silently revert cert sharing.
	var storage map[string]any
	if eps := EtcdStorageEndpoints(); len(eps) > 0 {
		storage = storageEtcdConfig(eps)
	}
	body, err := routes.renderCaddyConfigWith("unix//"+s.adminSocket, storage)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes.Equal(body, s.currentBytes) {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://caddy/load", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /load: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		// Caddy returns its error envelope as JSON; try to surface
		// the actual cause rather than the bare status code.
		var cErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &cErr) == nil && cErr.Error != "" {
			return fmt.Errorf("caddy reject (HTTP %d): %s", resp.StatusCode, cErr.Error)
		}
		return fmt.Errorf("caddy reject (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
	s.currentBytes = body
	log.Printf("weft-agent proxy: applied %d route(s)", len(routes))
	return nil
}

// Close gracefully shuts down the Caddy subprocess. Idempotent.
func (s *Supervisor) Close() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	// SIGTERM gives Caddy a chance to flush its access logs and
	// release the admin socket cleanly. We bound the wait so a
	// wedged caddy doesn't block agent shutdown.
	_ = s.cmd.Process.Signal(syscallSIGTERM)
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	_ = os.Remove(s.adminSocket)
	return nil
}

// defaultRuntimeDir returns $XDG_RUNTIME_DIR, falling back to /tmp when
// unset (macOS / containers without systemd-user).
func defaultRuntimeDir() string {
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return r
	}
	return os.TempDir()
}
