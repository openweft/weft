package wgtransport

import (
	"fmt"
	"log"
	"net"
	"net/netip"

	"golang.zx2c4.com/wireguard/device"
)

// ServerConfig holds the WireGuard server configuration.
type ServerConfig struct {
	// PrivateKeyPath is the path to a base64-encoded Curve25519 private key
	// (32 bytes, one line). Generated on first start if the file does not
	// exist.
	PrivateKeyPath string
	// LocalIP is this node's address on the overlay (e.g. 10.0.0.1).
	LocalIP netip.Addr
	// ListenPort is the UDP underlay port. Zero binds to an ephemeral port.
	ListenPort uint16
	// Peers lists the authorized clients. Used as-is when PeersPath is empty.
	Peers []Peer
	// PeersPath is an optional path to a peer file (see loadPeersFile). When
	// non-empty, peers from disk are appended to Peers.
	PeersPath string
	// MTU is the overlay MTU. Zero selects the default (1420).
	MTU int
	// Logger receives device-level messages. Defaults to log.Default().
	Logger *log.Logger
}

// ListenWireGuard brings up a userspace WireGuard device, listens for TCP
// connections on addr (an "ip:port" on the overlay), and returns a
// net.Listener suitable for grpc.Server.Serve. Closing the returned listener
// also tears down the WireGuard device.
func ListenWireGuard(addr string, cfg ServerConfig) (net.Listener, error) {
	priv, err := loadOrCreatePrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}

	peers := cfg.Peers
	if cfg.PeersPath != "" {
		extra, err := loadPeersFile(cfg.PeersPath)
		if err != nil {
			return nil, fmt.Errorf("peers file: %w", err)
		}
		peers = append(peers, extra...)
	}

	dev, tnet, err := bringUpDevice(priv, cfg.LocalIP, cfg.ListenPort, peers, cfg.MTU, cfg.Logger)
	if err != nil {
		return nil, err
	}

	tcpAddr, err := parseOverlayAddr(addr)
	if err != nil {
		dev.Close()
		return nil, err
	}
	lis, err := tnet.ListenTCP(tcpAddr)
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("listen overlay %s: %w", addr, err)
	}
	return &wgListener{Listener: lis, dev: dev}, nil
}

// wgListener wraps a netstack TCP listener so closing it also tears down the
// underlying WireGuard device.
type wgListener struct {
	net.Listener
	dev *device.Device
}

func (l *wgListener) Close() error {
	err := l.Listener.Close()
	l.dev.Close()
	return err
}
