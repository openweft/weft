// Package weftclient is the shared gRPC client surface for weft
// consumers — weft, weft-microvm, and any future tool that needs to talk to
// the daemon. It centralises:
//
//   - dialing (default ~/.weft/weft.sock, optional SSH transport)
//   - the proto-state → human-string mapping
//   - human-byte formatting
//
// Anything *display-flavoured* (table rendering, JSON shape) stays
// in each tool — those legitimately differ between the Docker-style
// weft-microvm output and the VZ-style weft output, and forcing one shape on
// both would just push complexity into formatting options.
//
// Stability: this is an exported module; backwards-compatible
// additions only — breaking changes need a new major version.
package weftclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sshtransport "github.com/grpc-transports/ssh"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DefaultSocket returns weft's default Unix-socket path
// (~/.weft/weft.sock). Falls back to /tmp/weft.sock when the user's
// home directory cannot be resolved — same behaviour weft prints at
// startup.
func DefaultSocket() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/tmp/weft.sock"
	}
	return filepath.Join(home, ".weft", "weft.sock")
}

// Options holds knobs for Dial / Client. Built via the functional
// option helpers below; zero-value is the local-unix-socket happy
// path.
type Options struct {
	timeout   time.Duration
	sshSocket string
	sshKey    string
	transport grpc.DialOption // custom transport (e.g. WireGuard); nil = default
	target    string          // gRPC target paired with transport
}

// Option is the functional-options sugar; Dial / Client take a
// variadic slice.
type Option func(*Options)

// WithTimeout overrides the default 3 s dial deadline.
func WithTimeout(d time.Duration) Option {
	return func(o *Options) { o.timeout = d }
}

// WithSSH switches the transport to an SSH-tunnelled connection
// via the grpc-transports/ssh package. `sshSocket` is the Unix
// socket weft's SSH server listens on (default ~/.weft/weft-ssh.sock
// when empty); `sshKey` is a private-key path. Setting `sshKey =
// ""` reverts to the local-socket happy path.
func WithSSH(sshSocket, sshKey string) Option {
	return func(o *Options) {
		o.sshSocket = sshSocket
		o.sshKey = sshKey
	}
}

// WithDialOption routes the connection through a caller-supplied transport
// dial option instead of the default local socket — used to reach a target
// that isn't weft's Unix socket, e.g. a micro-VM's gRPC endpoint over a
// WireGuard overlay. `target` is the gRPC dial target reached through `opt`
// (the opt's own dialer determines the real endpoint, so target is typically
// a passthrough address). Takes precedence over WithSSH and the local socket.
//
// Transport adapters such as weft-client/wgdial build this; keeping the heavy
// transport dependency in that subpackage means callers that only use the
// local socket or SSH never pull it in.
func WithDialOption(target string, opt grpc.DialOption) Option {
	return func(o *Options) {
		o.transport = opt
		o.target = target
	}
}

// Dial opens a gRPC connection to weft. An empty socketPath
// resolves to DefaultSocket(); SSH transport kicks in only when
// WithSSH(_, key != "") is passed. Caller closes the returned
// ClientConn.
func Dial(socketPath string, opts ...Option) (*grpc.ClientConn, error) {
	o := Options{timeout: 3 * time.Second}
	for _, fn := range opts {
		fn(&o)
	}
	if socketPath == "" {
		socketPath = DefaultSocket()
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	// Bearer interceptor: reads the on-disk token cache on every
	// RPC and stamps Authorization onto outgoing metadata. Empty
	// cache (= operator hasn't run `weft login`) means we just send
	// no header, which is what dev-mode weft expects.
	bearer := CachedTokenSource()
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithUnaryInterceptor(BearerInterceptor(bearer)),
		grpc.WithStreamInterceptor(BearerStreamInterceptor(bearer)),
	}
	if o.transport != nil {
		target := o.target
		if target == "" {
			target = "passthrough:///vm"
		}
		return grpc.DialContext(ctx, target,
			append([]grpc.DialOption{o.transport}, dialOpts...)...,
		)
	}
	if o.sshKey != "" {
		sshSock := o.sshSocket
		if sshSock == "" {
			home, _ := os.UserHomeDir()
			sshSock = filepath.Join(home, ".weft", "weft-ssh.sock")
		}
		sshOpt, err := sshtransport.DialOption("unix:"+sshSock, o.sshKey, "")
		if err != nil {
			return nil, fmt.Errorf("ssh dial option: %w", err)
		}
		return grpc.DialContext(ctx, "passthrough:///weft",
			append([]grpc.DialOption{sshOpt}, dialOpts...)...,
		)
	}
	// "tcp:host:port" target — plain TCP gRPC, used by cross-host bring-up
	// where the seed agent runs an explicit --tcp-listen listener. Dev-only
	// (no TLS); production should use the SSH transport once sshtransport
	// gains cross-host support. Bearer-token auth still flows through the
	// standard interceptor chain so OIDC-protected deploys work the same way.
	if strings.HasPrefix(socketPath, "tcp:") {
		return grpc.DialContext(ctx, strings.TrimPrefix(socketPath, "tcp:"), dialOpts...)
	}
	return grpc.DialContext(ctx, "unix:"+socketPath, dialOpts...)
}

// Client is the typed-client convenience over Dial — most callers
// want this. Caller closes the returned ClientConn.
func Client(socketPath string, opts ...Option) (weftv1.WeftAgentClient, *grpc.ClientConn, error) {
	conn, err := Dial(socketPath, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to weft at %s: %w", socketPath, err)
	}
	return weftv1.NewWeftAgentClient(conn), conn, nil
}

// StateString maps the proto VMState enum to a stable lowercase
// label. Lowercase ("running") is the Unix-conventional form weft
// has used since the start; weft-microvm matches that.
func StateString(s weftv1.VMState) string {
	switch s {
	case weftv1.VMState_VM_STATE_RUNNING:
		return "running"
	case weftv1.VMState_VM_STATE_STOPPED:
		return "stopped"
	case weftv1.VMState_VM_STATE_NOT_CREATED:
		return "not-created"
	case weftv1.VMState_VM_STATE_ERROR:
		return "error"
	default:
		return "unknown"
	}
}

// HumanBytes formats a byte count using binary prefixes (KiB, MiB,
// GiB…). Same algorithm weft has shipped since v0; centralising it
// means a future change to display rules (decimal prefixes,
// localisation, …) lands in one place.
func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
