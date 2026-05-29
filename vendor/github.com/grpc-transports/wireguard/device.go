package wgtransport

import (
	"encoding/hex"
	"fmt"
	"log"
	"net/netip"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const defaultMTU = 1420

// bringUpDevice creates a userspace WireGuard device backed by an in-process
// gVisor netstack TUN. The returned *netstack.Net exposes Dial/Listen methods
// scoped to the overlay; the *device.Device must be closed by the caller.
func bringUpDevice(
	privateKey []byte,
	localIP netip.Addr,
	listenPort uint16,
	peers []Peer,
	mtu int,
	logger *log.Logger,
) (*device.Device, *netstack.Net, error) {
	if mtu <= 0 {
		mtu = defaultMTU
	}
	if !localIP.IsValid() {
		return nil, nil, fmt.Errorf("localIP must be set")
	}

	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{localIP}, nil, mtu)
	if err != nil {
		return nil, nil, fmt.Errorf("create netstack tun: %w", err)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), newDeviceLogger(logger))

	cfg, err := buildUAPIConfig(privateKey, listenPort, peers)
	if err != nil {
		dev.Close()
		return nil, nil, err
	}
	if err := dev.IpcSet(cfg); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("configure wireguard device: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("bring up wireguard device: %w", err)
	}
	return dev, tnet, nil
}

// buildUAPIConfig renders a wireguard-go UAPI string from the given config.
// Hex encoding is required by the UAPI protocol.
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
		// Silent by default: WireGuard's per-routine chatter is noise unless
		// the caller asked for it.
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
