// server.go — the federation-lite read-only HTTP endpoint. Per
// docs/design/federation.md §3a, each cluster's weft-agent exposes
// `/cluster-info` returning the signed FederationManifest. Peers
// discover state by polling this URL on a 30 s cadence (see
// poller.go) — federation-lite is HTTP-pull, not gRPC ; the
// pull-model memory (`openweft_pull_model`) drives the choice.
//
// The server is intentionally tiny : one route, JSON body, ed25519
// signature carried in the `X-Cluster-Signature` header (hex-encoded
// so curl + jq operators can read it without binary-frame hassles).
// No TLS termination here — the agent process owns its own listener
// and operators are expected to front it with the reverse proxy
// (Caddy via `weft agent --proxy`) or a load balancer. Mounting on
// http.DefaultServeMux is avoided so multiple Server instances can
// coexist in tests without leaking handlers.

package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// SignatureHeader is the response header the federation manifest's
// ed25519 signature ships in. Lowercase canonical form on the wire,
// `X-Cluster-Signature` after Go's CanonicalMIMEHeaderKey transform.
const SignatureHeader = "X-Cluster-Signature"

// ClusterInfoPath is the single route the federation-lite endpoint
// exposes. Kept as a constant so the poller and the server agree on
// the URL suffix without copy-paste drift.
const ClusterInfoPath = "/cluster-info"

// ManifestProvider is the seam between weft-agent's manifest store
// and the federation HTTP server. The agent implementation reads
// from local etcd / file storage ; tests substitute a hand-built
// manifest. The returned manifest is treated as immutable — the
// server marshals + signs it on every request, which is fine at the
// design's volume (one peer × 30 s = ~3000 reqs/day).
type ManifestProvider interface {
	Manifest(ctx context.Context) (*FederationManifest, error)
}

// StaticManifest is a ManifestProvider that returns the same
// manifest on every call. Used by tests and by the single-cluster
// bootstrap path where the manifest only changes via `weft federation
// join/leave` (i.e. a config reload, not a live mutation).
type StaticManifest struct {
	M *FederationManifest
}

// Manifest implements ManifestProvider.
func (s StaticManifest) Manifest(_ context.Context) (*FederationManifest, error) {
	if s.M == nil {
		return nil, errors.New("federation: StaticManifest.M is nil")
	}
	return s.M, nil
}

// Server is the federation-lite HTTP server. Construct via NewServer
// then call Start (or use Handler() to mount on a caller-owned mux).
// Listen address comes from the operator via `WEFT_FEDERATION_LISTEN`
// or `federation { listen = ":9102" }` in weft.hcl — both default to
// disabled, federation is opt-in.
type Server struct {
	// Provider returns the manifest to serve on each /cluster-info
	// request. Required.
	Provider ManifestProvider
	// PrivateKey signs each response. Pinned to ed25519 ; the JOSE
	// alg-confusion footgun is exactly the surface we want to avoid
	// in a freshly-deployed federation. Required.
	PrivateKey ed25519.PrivateKey

	// listener + server are populated by Start. Tracked so Stop can
	// shut the underlying net.Listener even when Serve is blocked on
	// http.Server.Serve (which doesn't close on its own).
	mu       sync.Mutex
	listener net.Listener
	srv      *http.Server
}

// Handler returns the http.Handler the Server exposes. Useful when
// the operator wants to mount /cluster-info on an existing mux (e.g.
// the metrics endpoint) rather than dedicating a port. The handler
// is safe for concurrent calls.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(ClusterInfoPath, http.HandlerFunc(s.serveClusterInfo))
	return mux
}

func (s *Server) serveClusterInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Provider == nil {
		http.Error(w, "federation: no manifest provider configured", http.StatusInternalServerError)
		return
	}
	if len(s.PrivateKey) != ed25519.PrivateKeySize {
		http.Error(w, "federation: missing or malformed signing key", http.StatusInternalServerError)
		return
	}
	m, err := s.Provider.Manifest(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("federation: load manifest: %v", err), http.StatusInternalServerError)
		return
	}
	b, err := m.Marshal()
	if err != nil {
		http.Error(w, fmt.Sprintf("federation: marshal manifest: %v", err), http.StatusInternalServerError)
		return
	}
	sig := ed25519.Sign(s.PrivateKey, b)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(SignatureHeader, hex.EncodeToString(sig))
	// Best-effort write — if the client hung up between header and
	// body there's no useful recovery here.
	_, _ = w.Write(b)
}

// Start binds the server to addr and serves on a fresh goroutine.
// Returns nil on a successful bind ; the actual Serve goroutine logs
// terminal errors via the http.Server's ErrorLog (which the agent's
// boot wiring points at its own logger). Caller is expected to call
// Stop before process exit.
func (s *Server) Start(addr string) error {
	if addr == "" {
		return errors.New("federation: empty listen address")
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("federation: listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second, // /cluster-info is unauthenticated ; bound slowloris
	}
	s.mu.Lock()
	s.listener = lis
	s.srv = srv
	s.mu.Unlock()
	go func() {
		// Serve returns ErrServerClosed on Stop ; anything else is
		// an unexpected listener death (port stolen, fd exhaustion).
		// The agent's boot logger picks it up via srv.ErrorLog.
		_ = srv.Serve(lis)
	}()
	return nil
}

// Addr returns the server's bound address. Useful for tests that
// pass `:0` and need to discover the actual port. Returns "" when
// Start has not been called or already returned.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Stop gracefully shuts the underlying http.Server. Safe to call
// multiple times ; idempotent. The 2 s timeout is short on purpose
// — /cluster-info handlers do no I/O beyond a manifest read, so
// anything still in flight after 2 s is stuck and worth killing.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.listener = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	shutCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}
