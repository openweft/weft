package wgtransport

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"

	"google.golang.org/grpc"
)

// ClientConfig holds the WireGuard client configuration.
type ClientConfig struct {
	// Backend selects the WireGuard implementation. See ServerConfig.Backend.
	Backend Backend
	// InterfaceName, when Backend=BackendKernel, names the wg* netdev
	// the bring-up creates. Empty → auto-generated. Ignored for userspace.
	InterfaceName string
	// PrivateKey is a base64-encoded Curve25519 private key supplied inline.
	// When set it takes precedence over PrivateKeyPath — handy for callers
	// that already hold the key (e.g. coordinates handed out by a control
	// plane) and don't want to stage a file.
	PrivateKey string
	// PrivateKeyPath is the path to a base64-encoded Curve25519 private key.
	// Generated on first start if missing. Ignored when PrivateKey is set.
	PrivateKeyPath string
	// LocalIP is this node's address on the overlay (e.g. 10.0.0.2).
	LocalIP netip.Addr
	// Peer is the server peer. Peer.Endpoint must be set to the server's
	// underlay UDP address ("host:port").
	Peer Peer
	// MTU is the overlay MTU. Zero selects the default.
	MTU int
	// Logger receives device-level messages. Defaults to log.Default().
	Logger *log.Logger
}

// BringUpClient mirrors BringUp for ClientConfig: brings the WireGuard
// device up, peers it with the single server peer, and returns a Closer.
// No gRPC dial option is added on top — callers that just need the wg
// interface (data path only) use this.
func BringUpClient(cfg ClientConfig) (Closer, error) {
	_, c, err := bringUpFromClientConfig(cfg)
	return c, err
}

// bringUpFromClientConfig is the shared internal — see bringUpFromServerConfig.
func bringUpFromClientConfig(cfg ClientConfig) (wgNet, Closer, error) {
	if cfg.Peer.Endpoint == "" {
		return nil, nil, fmt.Errorf("ClientConfig.Peer.Endpoint must be set")
	}

	priv, err := resolvePrivateKey(cfg.PrivateKey, cfg.PrivateKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("private key: %w", err)
	}

	wgnet, err := bringUpDevice(cfg.Backend, cfg.InterfaceName, priv, cfg.LocalIP, 0, []Peer{cfg.Peer}, cfg.MTU, cfg.Logger)
	if err != nil {
		return nil, nil, err
	}
	return wgnet, wgnet, nil
}

// DialOption returns a grpc.DialOption that tunnels every gRPC connection
// through a fresh WireGuard device to the overlay address addr
// ("ip:port" on the overlay).
//
// One device is created per DialOption invocation; reuse the returned
// option for multiple grpc.Dial calls rather than calling DialOption
// repeatedly. For BackendKernel, the wg* interface stays up until process
// exit (callers that need explicit teardown should switch to a higher-
// level wrapper that exposes a Close hook).
func DialOption(addr string, cfg ClientConfig) (grpc.DialOption, error) {
	wgnet, _, err := bringUpFromClientConfig(cfg)
	if err != nil {
		return nil, err
	}

	return grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return wgnet.DialContext(ctx, addr)
	}), nil
}
