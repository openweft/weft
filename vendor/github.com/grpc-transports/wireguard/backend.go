package wgtransport

// backend.go — Backend selection.
//
// Two backends ship: userspace (wireguard-go + gVisor netstack, default,
// works on any OS) and kernel (Linux kernel WireGuard via wgctrl + netlink,
// requires CAP_NET_ADMIN). The kernel path is the right choice inside a
// microVM whose kernel already has CONFIG_WIREGUARD compiled in: the
// userspace stack would re-implement what the kernel already does, eat
// more CPU on encrypt/decrypt, and prevent the OS-level routing/firewall
// from seeing the overlay traffic.

import (
	"context"
	"net"
)

// Backend selects which WireGuard implementation drives the data path.
type Backend int

const (
	// BackendUserspace runs wireguard-go + gVisor netstack in-process.
	// Works on any OS, needs no privileges, but everything (TCP/IP,
	// firewall, routing) is in the binary's address space — invisible
	// to host tools like `iptables`, `ip route`, `ss`.
	BackendUserspace Backend = iota

	// BackendKernel drives the Linux kernel's WireGuard module via
	// wgctrl (netlink) for device config and netlink for interface + IP
	// + route setup. Requires CAP_NET_ADMIN; the resulting wg* interface
	// is a regular netdev visible to host tools (`ip`, `iptables`, `ss`).
	// Linux-only — non-Linux builds return an error at runtime if this
	// backend is requested.
	BackendKernel
)

// String renders the Backend for logs and validation messages.
func (b Backend) String() string {
	switch b {
	case BackendUserspace:
		return "userspace"
	case BackendKernel:
		return "kernel"
	default:
		return "unknown"
	}
}

// wgNet is what server.go and client.go consume — a Listen + Dial pair
// scoped to one overlay, plus a Close that tears the device down. Both
// backends implement it; callers don't see which.
//
// ListenTCP and DialContext take an "ip:port" string on the overlay (same
// format as net.Dial's address). Implementations are responsible for
// parsing it.
type wgNet interface {
	ListenTCP(addr string) (net.Listener, error)
	DialContext(ctx context.Context, addr string) (net.Conn, error)
	Close() error
}

// Closer is the public handle BringUp / BringUpClient return: just a Close
// hook that tears the WireGuard device down. Used by guests that only need
// the data path (kernel wg* netdev plumbed for the rest of the OS to use),
// without the gRPC listener/dialer this package layers on top.
type Closer interface {
	Close() error
}
