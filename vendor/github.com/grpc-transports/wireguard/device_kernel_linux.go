//go:build linux

package wgtransport

// device_kernel_linux.go — kernel-mode WireGuard backend.
//
// Drives the Linux kernel's WireGuard module (requires CAP_NET_ADMIN):
//
//   1. netlink: create a wg<random> interface of kind=wireguard
//   2. wgctrl:  set private key, listen port, peers on it
//   3. netlink: assign LocalIP to the interface
//   4. netlink: add routes for every peer's AllowedIPs via this interface
//   5. netlink: bring the interface UP
//
// ListenTCP/DialContext then go through the OS's normal TCP/IP stack —
// the kernel handles encrypt/decrypt on the wire, and the application
// sees a regular net.Conn. This is what `wg-quick` does internally,
// minus the bash.
//
// Side-effects: a real netdev exists for the lifetime of the wgNet.
// Close() deletes the interface (cleanup is best-effort — kernel will
// also clean up if the process exits).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// kernelNet is the wgNet that drives a kernel wg* interface.
type kernelNet struct {
	ifname string
	link   netlink.Link
	wg     *wgctrl.Client
	logger *log.Logger
}

func (k *kernelNet) ListenTCP(addr string) (net.Listener, error) {
	// Standard Go listener bound on the overlay IP — the kernel routes
	// incoming WG UDP traffic into the wg* interface, decrypts, and the
	// inner TCP stream hits this listener like any other.
	return net.Listen("tcp", addr)
}

func (k *kernelNet) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (k *kernelNet) Close() error {
	if k.wg != nil {
		_ = k.wg.Close()
	}
	if k.link != nil {
		// LinkDel may fail if the interface was already removed (e.g.
		// the process holding it died and netns cleanup beat us to it).
		// Log + ignore — the goal state ("interface gone") is reached
		// either way.
		if err := netlink.LinkDel(k.link); err != nil && k.logger != nil {
			k.logger.Printf("wgtransport: kernel: link del %s: %v (already gone?)", k.ifname, err)
		}
	}
	return nil
}

// bringUpKernel performs the 5-step bring-up sketched at the top of the
// file. Each step is idempotent against an already-present interface
// when ifname is non-empty — useful for operator-managed wg0 setups
// where the interface should persist across restarts.
func bringUpKernel(
	ifname string,
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
	if len(privateKey) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes, got %d", len(privateKey))
	}
	if ifname == "" {
		ifname = randomIfname()
	}

	// 1. Create or reuse the wg interface.
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		// LinkNotFoundError isn't a sentinel in the netlink lib — fall
		// through to create on any error, then if create also fails we
		// surface that more useful message.
		lnk := &netlink.GenericLink{
			LinkAttrs: netlink.LinkAttrs{Name: ifname, MTU: mtu},
			LinkType:  "wireguard",
		}
		if addErr := netlink.LinkAdd(lnk); addErr != nil {
			return nil, fmt.Errorf("kernel backend: create %s: %w (need CAP_NET_ADMIN?)", ifname, addErr)
		}
		link, err = netlink.LinkByName(ifname)
		if err != nil {
			return nil, fmt.Errorf("kernel backend: post-create lookup %s: %w", ifname, err)
		}
	}

	cleanup := func() {
		if delErr := netlink.LinkDel(link); delErr != nil && logger != nil {
			logger.Printf("wgtransport: kernel: cleanup link del %s: %v", ifname, delErr)
		}
	}

	// 2. Configure the device (private key, listen port, peers).
	wg, err := wgctrl.New()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("kernel backend: open wgctrl: %w", err)
	}
	devCfg, err := buildWGCtrlConfig(privateKey, listenPort, peers)
	if err != nil {
		_ = wg.Close()
		cleanup()
		return nil, err
	}
	if err := wg.ConfigureDevice(ifname, devCfg); err != nil {
		_ = wg.Close()
		cleanup()
		return nil, fmt.Errorf("kernel backend: configure %s: %w", ifname, err)
	}

	// 3. Assign the overlay IP to the interface. /32 (or /128) avoids
	// hijacking traffic destined elsewhere — operators who want a
	// broader on-link subnet should add routes after the fact.
	bits := 32
	if localIP.Is6() {
		bits = 128
	}
	prefix := netip.PrefixFrom(localIP, bits)
	if err := addrAdd(link, prefix); err != nil {
		_ = wg.Close()
		cleanup()
		return nil, fmt.Errorf("kernel backend: addr add %s on %s: %w", prefix, ifname, err)
	}

	// 4. Route peer AllowedIPs through the interface.
	for _, p := range peers {
		for _, aip := range p.AllowedIPs {
			if err := routeAdd(link, aip); err != nil {
				_ = wg.Close()
				cleanup()
				return nil, fmt.Errorf("kernel backend: route %s via %s: %w", aip, ifname, err)
			}
		}
	}

	// 5. Bring the interface UP.
	if err := netlink.LinkSetUp(link); err != nil {
		_ = wg.Close()
		cleanup()
		return nil, fmt.Errorf("kernel backend: link up %s: %w", ifname, err)
	}

	// One-shot warm-up: WireGuard handshake hasn't happened yet —
	// callers' first Dial will trigger it. A 100ms pause here gives
	// the keepalive scheduler time to install its timer before the
	// caller starts sending application traffic.
	time.Sleep(100 * time.Millisecond)
	return &kernelNet{
		ifname: ifname,
		link:   link,
		wg:     wg,
		logger: logger,
	}, nil
}

// addrAdd is the small wrapper around netlink.AddrAdd that turns a
// netip.Prefix into the netlink-flavoured address. Pulled out of
// bringUpKernel so the latter reads as a recipe.
func addrAdd(link netlink.Link, prefix netip.Prefix) error {
	ipnet := &net.IPNet{
		IP:   net.ParseIP(prefix.Addr().String()),
		Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
	}
	return netlink.AddrAdd(link, &netlink.Addr{IPNet: ipnet})
}

// routeAdd installs an on-link route for the peer's AllowedIP through
// the wg interface. Scope is "link" so the kernel doesn't try to
// resolve a gateway (WireGuard is gateway-less).
func routeAdd(link netlink.Link, prefix netip.Prefix) error {
	ipnet := &net.IPNet{
		IP:   net.ParseIP(prefix.Addr().String()),
		Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
	}
	return netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       ipnet,
		Scope:     netlink.SCOPE_LINK,
	})
}

// buildWGCtrlConfig translates our Peer slice into the wgctrl-go config
// shape, mirroring buildUAPIConfig (used by the userspace backend).
func buildWGCtrlConfig(privateKey []byte, listenPort uint16, peers []Peer) (wgtypes.Config, error) {
	priv, err := wgtypes.NewKey(privateKey)
	if err != nil {
		return wgtypes.Config{}, fmt.Errorf("wgtypes private key: %w", err)
	}
	cfg := wgtypes.Config{
		PrivateKey:   &priv,
		ReplacePeers: true,
	}
	if listenPort != 0 {
		lp := int(listenPort)
		cfg.ListenPort = &lp
	}
	for i, p := range peers {
		pubBytes, err := decodeKey(p.PublicKey)
		if err != nil {
			return wgtypes.Config{}, fmt.Errorf("peer %d: %w", i, err)
		}
		pub, err := wgtypes.NewKey(pubBytes)
		if err != nil {
			return wgtypes.Config{}, fmt.Errorf("peer %d pubkey: %w", i, err)
		}
		pc := wgtypes.PeerConfig{
			PublicKey:         pub,
			ReplaceAllowedIPs: true,
		}
		for _, prefix := range p.AllowedIPs {
			ipnet := net.IPNet{
				IP:   net.ParseIP(prefix.Addr().String()),
				Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
			}
			pc.AllowedIPs = append(pc.AllowedIPs, ipnet)
		}
		if p.Endpoint != "" {
			udp, err := net.ResolveUDPAddr("udp", p.Endpoint)
			if err != nil {
				return wgtypes.Config{}, fmt.Errorf("peer %d endpoint %q: %w", i, p.Endpoint, err)
			}
			pc.Endpoint = udp
		}
		if p.PersistentKeepalive != 0 {
			ka := time.Duration(p.PersistentKeepalive) * time.Second
			pc.PersistentKeepaliveInterval = &ka
		}
		cfg.Peers = append(cfg.Peers, pc)
	}
	return cfg, nil
}

// randomIfname generates "wg-<8 hex chars>" — short enough for the 15-byte
// Linux ifname limit (max 13 chars used → fits).
func randomIfname() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "wg-" + hex.EncodeToString(b[:])
}
