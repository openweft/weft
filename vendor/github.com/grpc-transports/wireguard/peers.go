package wgtransport

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Peer describes a single WireGuard peer reachable on the overlay.
type Peer struct {
	// PublicKey is the peer's Curve25519 public key, base64-encoded (32 bytes).
	PublicKey string
	// AllowedIPs lists the overlay prefixes routed through this peer.
	AllowedIPs []netip.Prefix
	// Endpoint is the optional underlay UDP endpoint ("host:port"). Required
	// on the client side; the server learns clients' endpoints from the
	// handshake.
	Endpoint string
	// PersistentKeepalive is the keepalive interval in seconds. Zero disables
	// keepalives.
	PersistentKeepalive uint16
}

// loadPeersFile parses a peer file. Each non-blank, non-comment line has the
// form:
//
//	<base64-pubkey> <allowed-ip>[,<allowed-ip>...] [<endpoint:port>] [<keepalive>]
//
// Comments start with '#'.
func loadPeersFile(path string) ([]Peer, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var peers []Peer
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		p, err := parsePeerLine(text)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		peers = append(peers, p)
	}
	return peers, scanner.Err()
}

func parsePeerLine(line string) (Peer, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return Peer{}, fmt.Errorf("expected at least <pubkey> <allowed-ips>")
	}
	if _, err := decodeKey(fields[0]); err != nil {
		return Peer{}, fmt.Errorf("pubkey: %w", err)
	}
	p := Peer{PublicKey: fields[0]}
	for _, cidr := range strings.Split(fields[1], ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return Peer{}, fmt.Errorf("allowed-ip %q: %w", cidr, err)
		}
		p.AllowedIPs = append(p.AllowedIPs, prefix)
	}
	if len(fields) >= 3 {
		p.Endpoint = fields[2]
	}
	if len(fields) >= 4 {
		ka, err := strconv.ParseUint(fields[3], 10, 16)
		if err != nil {
			return Peer{}, fmt.Errorf("keepalive %q: %w", fields[3], err)
		}
		p.PersistentKeepalive = uint16(ka)
	}
	return p, nil
}
