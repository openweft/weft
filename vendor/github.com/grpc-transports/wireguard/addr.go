package wgtransport

import (
	"fmt"
	"net"
	"net/netip"
)

// parseOverlayAddr parses an "ip:port" overlay address into a *net.TCPAddr
// suitable for netstack's ListenTCP / DialContextTCP.
func parseOverlayAddr(addr string) (*net.TCPAddr, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, fmt.Errorf("parse overlay addr %q: %w", addr, err)
	}
	return net.TCPAddrFromAddrPort(ap), nil
}
