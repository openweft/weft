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

// DialOption returns a grpc.DialOption that tunnels every gRPC connection
// through a fresh userspace WireGuard device to the overlay address addr
// ("ip:port" on the overlay).
//
// One device is created per DialOption invocation; reuse the returned option
// for multiple grpc.Dial calls rather than calling DialOption repeatedly.
func DialOption(addr string, cfg ClientConfig) (grpc.DialOption, error) {
	if cfg.Peer.Endpoint == "" {
		return nil, fmt.Errorf("ClientConfig.Peer.Endpoint must be set")
	}

	priv, err := resolvePrivateKey(cfg.PrivateKey, cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}

	dev, tnet, err := bringUpDevice(priv, cfg.LocalIP, 0, []Peer{cfg.Peer}, cfg.MTU, cfg.Logger)
	if err != nil {
		return nil, err
	}

	tcpAddr, err := parseOverlayAddr(addr)
	if err != nil {
		dev.Close()
		return nil, err
	}

	return grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return tnet.DialContextTCP(ctx, tcpAddr)
	}), nil
}
