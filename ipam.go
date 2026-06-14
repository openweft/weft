package weft

// ipam.go is the shared "next-free address in a CIDR" picker used
// by both the Port registry (CreatePort with spec.IP == "" wants
// the next free private IP) and the FloatingIP registry (same
// algorithm, different exclusion list).
//
// The picker walks the CIDR in lexicographic order skipping :
//   - the network address (first IP)
//   - the IPv4 broadcast address (last IP), never relevant on IPv6
//   - every address in `excluded` (passed by the caller : already-
//     allocated ports + gateway + DHCP server + the caller's own
//     reserved set).
//
// Pure : no IO, no mutex. Caller owns the exclusion list snapshot
// so concurrent allocators serialise via the caller's lock.

import (
	"fmt"
	"net/netip"
)

// PickFreeAddress returns the lowest-address IP in cidr not in
// excluded, as a bare IP string ("10.0.0.5"). Errors when :
//   - cidr is not parseable as a prefix
//   - the pool is exhausted (every host address is excluded)
//
// IPv4 /31 and /32 are degenerate (no broadcast / single host) ;
// the picker handles both correctly (a /32 yields the single IP
// if not excluded, otherwise "exhausted").
func PickFreeAddress(cidr string, excluded []string) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	taken := make(map[string]struct{}, len(excluded))
	for _, s := range excluded {
		if addr, err := netip.ParseAddr(s); err == nil {
			taken[addr.String()] = struct{}{}
		} else {
			// Keep the raw string as a fallback — better to over-
			// reject than to leak a colliding allocation.
			taken[s] = struct{}{}
		}
	}
	ip := prefix.Masked().Addr()
	first := true
	// /32 (IPv4) and /128 (IPv6) carry exactly one host : the
	// network address IS the host, no network/broadcast skip.
	hostBits := ip.BitLen() - prefix.Bits()
	skipFirstAndBroadcast := hostBits > 0
	for prefix.Contains(ip) {
		skip := false
		if skipFirstAndBroadcast {
			if first {
				skip = true // network address
			} else if ipIsBroadcast(ip, prefix) {
				skip = true
			}
		}
		if !skip {
			s := ip.String()
			if _, used := taken[s]; !used {
				return s, nil
			}
		}
		first = false
		next := ip.Next()
		if !next.IsValid() || !prefix.Contains(next) {
			break
		}
		ip = next
	}
	return "", fmt.Errorf("no free addresses in %s", cidr)
}

// ipIsBroadcast returns true when ip is the IPv4 all-1s host of
// prefix. /32 has no broadcast (it's a single host) ; IPv6 has
// no broadcast concept.
func ipIsBroadcast(ip netip.Addr, prefix netip.Prefix) bool {
	if !ip.Is4() || !prefix.Addr().Is4() {
		return false
	}
	bits := prefix.Bits()
	if bits >= 32 {
		return false
	}
	hostBits := 32 - bits
	masked := prefix.Masked().Addr().As4()
	var bcast [4]byte
	copy(bcast[:], masked[:])
	for i := 31; i >= 32-hostBits; i-- {
		byteIdx, bitIdx := i/8, uint(7-i%8)
		bcast[byteIdx] |= 1 << bitIdx
	}
	return netip.AddrFrom4(bcast) == ip
}
