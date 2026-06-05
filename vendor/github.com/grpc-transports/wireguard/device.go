package wgtransport

// device.go — userspace backend implementation.
//
// Builds a wireguard-go device backed by a gVisor netstack TUN; the
// returned wgNet exposes ListenTCP / DialContext that route through the
// in-process stack. Works on any OS, needs no privileges.

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const defaultMTU = 1420

// userspaceNet is the wgNet built on wireguard-go + netstack.
type userspaceNet struct {
	dev  *device.Device
	tnet *netstack.Net
}

func (u *userspaceNet) ListenTCP(addr string) (net.Listener, error) {
	tcpAddr, err := parseOverlayAddr(addr)
	if err != nil {
		return nil, err
	}
	return u.tnet.ListenTCP(tcpAddr)
}

func (u *userspaceNet) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	tcpAddr, err := parseOverlayAddr(addr)
	if err != nil {
		return nil, err
	}
	return u.tnet.DialContextTCP(ctx, tcpAddr)
}

func (u *userspaceNet) Close() error {
	u.dev.Close()
	return nil
}

// bringUpUserspace creates a userspace WireGuard device. Kept separate
// from the kernel path so backend selection can branch cleanly without
// either side importing the other's deps at runtime.
func bringUpUserspace(
	privateKey []byte,
	localIP netip.Addr,
	listenPort uint16,
	peers []Peer,
	mtu int,
	logger *log.Logger,
) (wgNet, error) {
	if mtu <= 0 {
		mtu = defaultMTU
	}
	if !localIP.IsValid() {
		return nil, fmt.Errorf("localIP must be set")
	}

	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{localIP}, nil, mtu)
	if err != nil {
		return nil, fmt.Errorf("create netstack tun: %w", err)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), newDeviceLogger(logger))

	cfg, err := buildUAPIConfig(privateKey, listenPort, peers)
	if err != nil {
		dev.Close()
		return nil, err
	}
	if err := dev.IpcSet(cfg); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure wireguard device: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("bring up wireguard device: %w", err)
	}
	return &userspaceNet{dev: dev, tnet: tnet}, nil
}

// buildUAPIConfig renders a wireguard-go UAPI string from the given
// config. Hex encoding is required by the UAPI protocol.
func buildUAPIConfig(privateKey []byte, listenPort uint16, peers []Peer) (string, error) {
	if len(privateKey) != 32 {
		return "", fmt.Errorf("private key must be 32 bytes, got %d", len(privateKey))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "private_key=%s\n", hex.EncodeToString(privateKey))
	if listenPort != 0 {
		fmt.Fprintf(&sb, "listen_port=%d\n", listenPort)
	}
	for i, p := range peers {
		pub, err := decodeKey(p.PublicKey)
		if err != nil {
			return "", fmt.Errorf("peer %d: %w", i, err)
		}
		fmt.Fprintf(&sb, "public_key=%s\n", hex.EncodeToString(pub))
		for _, prefix := range p.AllowedIPs {
			fmt.Fprintf(&sb, "allowed_ip=%s\n", prefix)
		}
		if p.Endpoint != "" {
			fmt.Fprintf(&sb, "endpoint=%s\n", p.Endpoint)
		}
		if p.PersistentKeepalive != 0 {
			fmt.Fprintf(&sb, "persistent_keepalive_interval=%d\n", p.PersistentKeepalive)
		}
	}
	return sb.String(), nil
}

func newDeviceLogger(logger *log.Logger) *device.Logger {
	if logger == nil {
		// Silent by default: WireGuard's per-routine chatter is noise
		// unless the caller asked for it.
		return &device.Logger{
			Verbosef: func(string, ...any) {},
			Errorf:   func(string, ...any) {},
		}
	}
	return &device.Logger{
		Verbosef: func(format string, args ...any) {
			logger.Printf("wgtransport: "+format, args...)
		},
		Errorf: func(format string, args ...any) {
			logger.Printf("wgtransport: ERROR "+format, args...)
		},
	}
}

// bringUpDevice is the backend-aware constructor server.go and client.go
// call. Branches on cfg.Backend; the kernel branch lives in a separate
// file gated by the linux build tag.
func bringUpDevice(
	backend Backend,
	ifname string,
	privateKey []byte,
	localIP netip.Addr,
	listenPort uint16,
	peers []Peer,
	mtu int,
	logger *log.Logger,
) (wgNet, error) {
	switch backend {
	case BackendUserspace:
		return bringUpUserspace(privateKey, localIP, listenPort, peers, mtu, logger)
	case BackendKernel:
		return bringUpKernel(ifname, privateKey, localIP, listenPort, peers, mtu, logger)
	default:
		return nil, fmt.Errorf("unknown backend: %v", backend)
	}
}
