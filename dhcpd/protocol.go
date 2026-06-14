package dhcpd

// Pure-Go DHCPv4 wire format (RFC 2131 + 2132).
//
// Why hand-rolled : weft's module tree carries a broken transitive
// (`go-compressions/matchlen v0.0.0`) that blocks `go mod tidy`
// from pulling `insomniacslk/dhcp`. DHCPv4 is a fixed-format
// protocol — header + magic cookie + TLV options terminated by 255
// — small enough to encode here cleanly and avoid the dep until
// the upstream gunk is resolved.
//
// This file is build-tag-free so the parser/builder unit tests run
// on the darwin dev box and the linux CI box alike. Only the
// socket wiring lives behind //go:build linux.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// BOOTP op codes (RFC 951 ; reused by DHCP).
const (
	opBootRequest byte = 1
	opBootReply   byte = 2
)

// Hardware type 1 = Ethernet (10Mb), reused for everything that
// looks like Ethernet on the wire.
const htypeEthernet byte = 1

// magicCookie is the 4-byte sentinel that separates the BOOTP
// header from the DHCP options block (RFC 2131 §3).
var magicCookie = [4]byte{99, 130, 83, 99}

// DHCP message types — option 53 single-byte value.
const (
	MsgDiscover byte = 1
	MsgOffer    byte = 2
	MsgRequest  byte = 3
	MsgDecline  byte = 4
	MsgAck      byte = 5
	MsgNak      byte = 6
	MsgRelease  byte = 7
	MsgInform   byte = 8
)

// Option codes we care about. The full DHCP options registry is
// large ; this server only emits / inspects a handful.
const (
	optPad              byte = 0
	optSubnetMask       byte = 1
	optRouter           byte = 3
	optDomainNameServer byte = 6
	optDomainName       byte = 15
	optRequestedIP      byte = 50
	optLeaseTime        byte = 51
	optMessageType      byte = 53
	optServerIdentifier byte = 54
	optParameterRequest byte = 55
	optEnd              byte = 255
)

// Fixed BOOTP header size before the magic cookie (RFC 951 / 2131).
const bootpHeaderSize = 236

// Packet is the parsed DHCPv4 message. We surface only the bits
// the server actually reads ; the rest is round-tripped through
// raw byte slices when needed.
type Packet struct {
	Op      byte    // BOOTREQUEST (1) / BOOTREPLY (2)
	Htype   byte    // 1 = Ethernet
	Hlen    byte    // 6 for Ethernet
	Hops    byte    // 0 unless a relay touched it
	Xid     uint32  // client-chosen transaction id ; we echo it
	Secs    uint16  // seconds since the client started DHCP
	Flags   uint16  // bit 15 = broadcast
	Ciaddr  [4]byte // client current address (RENEW)
	Yiaddr  [4]byte // "your" address (server fills this in OFFER/ACK)
	Siaddr  [4]byte // next server address (BOOTP next-stage, unused)
	Giaddr  [4]byte // gateway / relay address
	Chaddr  [16]byte
	Sname   [64]byte
	File    [128]byte
	Options map[byte][]byte // parsed TLV map ; key = option code
}

// MessageType reads option 53. Returns 0 when absent.
func (p *Packet) MessageType() byte {
	v, ok := p.Options[optMessageType]
	if !ok || len(v) == 0 {
		return 0
	}
	return v[0]
}

// RequestedIP reads option 50 (the IP the client wants confirmed
// in a REQUEST). Returns the zero netip.Addr when absent.
func (p *Packet) RequestedIP() netip.Addr {
	v, ok := p.Options[optRequestedIP]
	if !ok || len(v) != 4 {
		return netip.Addr{}
	}
	return netip.AddrFrom4([4]byte{v[0], v[1], v[2], v[3]})
}

// MACString formats the first Hlen bytes of Chaddr as a colon-
// separated lowercase MAC string. Falls back to 6 bytes when Hlen
// is unset (some clients zero it).
func (p *Packet) MACString() string {
	n := int(p.Hlen)
	if n == 0 || n > 16 {
		n = 6
	}
	const hex = "0123456789abcdef"
	out := make([]byte, n*3-1)
	for i := 0; i < n; i++ {
		out[i*3] = hex[p.Chaddr[i]>>4]
		out[i*3+1] = hex[p.Chaddr[i]&0x0f]
		if i < n-1 {
			out[i*3+2] = ':'
		}
	}
	return string(out)
}

// Parse decodes a DHCPv4 packet. Returns an error when the buffer
// is too short, the magic cookie is missing, or the option block is
// malformed. The parser is strict on framing but tolerates unknown
// option codes (they go into the Options map as-is).
func Parse(buf []byte) (*Packet, error) {
	if len(buf) < bootpHeaderSize+4 {
		return nil, fmt.Errorf("dhcpd: packet too short (%d bytes, need >=%d)", len(buf), bootpHeaderSize+4)
	}
	p := &Packet{
		Op:      buf[0],
		Htype:   buf[1],
		Hlen:    buf[2],
		Hops:    buf[3],
		Xid:     binary.BigEndian.Uint32(buf[4:8]),
		Secs:    binary.BigEndian.Uint16(buf[8:10]),
		Flags:   binary.BigEndian.Uint16(buf[10:12]),
		Options: make(map[byte][]byte),
	}
	copy(p.Ciaddr[:], buf[12:16])
	copy(p.Yiaddr[:], buf[16:20])
	copy(p.Siaddr[:], buf[20:24])
	copy(p.Giaddr[:], buf[24:28])
	copy(p.Chaddr[:], buf[28:44])
	copy(p.Sname[:], buf[44:108])
	copy(p.File[:], buf[108:236])

	// Magic cookie : the 4 bytes immediately after the BOOTP
	// header. Mismatch → not a DHCP packet (likely a stray BOOTP
	// request the server doesn't speak).
	cookie := buf[bootpHeaderSize : bootpHeaderSize+4]
	if cookie[0] != magicCookie[0] || cookie[1] != magicCookie[1] || cookie[2] != magicCookie[2] || cookie[3] != magicCookie[3] {
		return nil, errors.New("dhcpd: bad magic cookie (not a DHCP packet)")
	}

	// Options : type (1) + length (1) + value (N). Code 0 is pad
	// (no length), code 255 is end. Anything else without a full
	// length+value tail is malformed.
	opts := buf[bootpHeaderSize+4:]
	for i := 0; i < len(opts); {
		code := opts[i]
		if code == optEnd {
			break
		}
		if code == optPad {
			i++
			continue
		}
		i++
		if i >= len(opts) {
			return nil, fmt.Errorf("dhcpd: option %d truncated at length byte", code)
		}
		ln := int(opts[i])
		i++
		if i+ln > len(opts) {
			return nil, fmt.Errorf("dhcpd: option %d truncated: want %d bytes, have %d", code, ln, len(opts)-i)
		}
		val := make([]byte, ln)
		copy(val, opts[i:i+ln])
		p.Options[code] = val
		i += ln
	}
	return p, nil
}

// BuildReply assembles an OFFER / ACK / NAK from the incoming
// request packet `req` + the server-side answer. msgType picks
// among MsgOffer / MsgAck / MsgNak. serverID is the IP we put in
// option 54 ; for NAK the Lease is ignored (NAK has no yiaddr / no
// per-client config).
//
// The returned byte slice is sized exactly to the encoded packet ;
// callers Sendto it as-is on UDP/68.
func BuildReply(req *Packet, msgType byte, serverID netip.Addr, lease Lease) ([]byte, error) {
	if req == nil {
		return nil, errors.New("dhcpd: nil request packet")
	}
	if !serverID.IsValid() || !serverID.Is4() {
		return nil, errors.New("dhcpd: server identifier must be IPv4")
	}

	// Reserve a comfortable buffer ; growing is fine, the slice is
	// returned trimmed.
	out := make([]byte, bootpHeaderSize+4, 512)
	out[0] = opBootReply
	out[1] = req.Htype
	out[2] = req.Hlen
	out[3] = 0 // hops always 0 on the reply side
	binary.BigEndian.PutUint32(out[4:8], req.Xid)
	// secs stays 0 ; flags echoes the client's request (the
	// broadcast bit drives our reply path on the wire-write side).
	binary.BigEndian.PutUint16(out[10:12], req.Flags)
	// ciaddr is 0 in OFFER / ACK by convention (client doesn't have
	// a confirmed lease yet from the server's POV).
	// yiaddr = the lease (or 0 for NAK).
	if msgType != MsgNak {
		yi := lease.Yiaddr.As4()
		copy(out[16:20], yi[:])
	}
	// siaddr / giaddr left at 0 ; chaddr / sname / file copy-through.
	copy(out[28:44], req.Chaddr[:])
	// sname / file stay zero ; some clients reject reply siaddr but
	// not blank sname.

	// Magic cookie.
	copy(out[bootpHeaderSize:bootpHeaderSize+4], magicCookie[:])

	// Options. Order doesn't matter for correctness ; we emit in
	// a stable order so packet captures diff cleanly.
	out = appendOption(out, optMessageType, []byte{msgType})
	sid := serverID.As4()
	out = appendOption(out, optServerIdentifier, sid[:])

	if msgType != MsgNak {
		// Subnet mask : derive from prefix length.
		mask := prefixToMask(lease.SubnetMaskBits)
		out = appendOption(out, optSubnetMask, mask[:])

		if lease.Router.IsValid() && lease.Router.Is4() {
			r := lease.Router.As4()
			out = appendOption(out, optRouter, r[:])
		}
		if len(lease.DNSServers) > 0 {
			dns := make([]byte, 0, 4*len(lease.DNSServers))
			for _, ns := range lease.DNSServers {
				b := ns.As4()
				dns = append(dns, b[:]...)
			}
			out = appendOption(out, optDomainNameServer, dns)
		}
		if lease.Domain != "" {
			out = appendOption(out, optDomainName, []byte(lease.Domain))
		}

		ltime := lease.LeaseTime
		if ltime == 0 {
			ltime = defaultLeaseSeconds
		}
		secs := uint32(ltime.Seconds())
		var lt [4]byte
		binary.BigEndian.PutUint32(lt[:], secs)
		out = appendOption(out, optLeaseTime, lt[:])
	}

	out = append(out, optEnd)
	return out, nil
}

// appendOption emits one TLV. Length is bounded to 255 (the wire
// width of the length byte) ; oversized values are silently
// truncated — none of the options we emit ever approach that
// limit in practice (DNS list of 63+ servers, domain >255 chars).
func appendOption(buf []byte, code byte, val []byte) []byte {
	if len(val) > 255 {
		val = val[:255]
	}
	buf = append(buf, code, byte(len(val)))
	buf = append(buf, val...)
	return buf
}

// prefixToMask renders a /N prefix length as the 4-byte netmask
// (e.g. 24 → {255,255,255,0}).
func prefixToMask(bits int) [4]byte {
	if bits <= 0 {
		return [4]byte{}
	}
	if bits > 32 {
		bits = 32
	}
	var m uint32 = 0xffffffff << (32 - uint(bits))
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], m)
	return out
}

// defaultLeaseSeconds is the option-51 fallback when Lease.LeaseTime
// is zero. Matches the doc-comment on Lease.LeaseTime.
const defaultLeaseSeconds = time.Hour

// Decision describes the server's intended response after running
// the DHCP state machine on a parsed inbound packet. It's the
// platform-agnostic core of the server loop — the linux wire code
// just hands a parsed packet to Decide and writes whatever bytes
// come back. Pulling the logic out lets the unit tests verify the
// state machine on darwin without binding a real UDP socket.
type Decision struct {
	// Reply is the wire-encoded payload to send. Nil means "drop
	// this packet silently" (unknown MAC, ignored message type,
	// malformed lease from Source).
	Reply []byte
	// MsgType is the DHCP message type carried in Reply (OFFER /
	// ACK / NAK). Zero when Reply is nil. Exposed for logging /
	// tests, not used on the wire (it's already inside Reply).
	MsgType byte
	// MAC is the parsed client MAC, surfaced for logging.
	MAC string
}

// Decide runs the server's state machine on a parsed inbound
// packet :
//
//   - Anything but BOOTREQUEST → drop (echo of our own reply).
//   - Resolve(mac) returns false → drop (unknown MAC).
//   - Lease.Validate fails → drop (Source bug ; logged by caller).
//   - DISCOVER → OFFER.
//   - REQUEST with mismatched option-50 → NAK.
//   - REQUEST otherwise → ACK.
//   - Anything else (DECLINE/RELEASE/INFORM/unknown) → drop.
//
// Returns a Decision the caller can inspect + send.
func Decide(pkt *Packet, opts Options) (Decision, error) {
	if pkt == nil {
		return Decision{}, errors.New("dhcpd: nil packet")
	}
	d := Decision{MAC: pkt.MACString()}
	if pkt.Op != opBootRequest {
		return d, nil
	}
	mt := pkt.MessageType()
	if mt != MsgDiscover && mt != MsgRequest {
		return d, nil
	}
	lease, ok := opts.Source.Resolve(d.MAC)
	if !ok {
		return d, nil
	}
	if err := lease.Validate(); err != nil {
		return d, fmt.Errorf("invalid lease from Source: %w", err)
	}

	switch mt {
	case MsgDiscover:
		raw, err := BuildReply(pkt, MsgOffer, opts.ServerIP, lease)
		if err != nil {
			return d, err
		}
		d.Reply = raw
		d.MsgType = MsgOffer
	case MsgRequest:
		if req := pkt.RequestedIP(); req.IsValid() && req != lease.Yiaddr {
			raw, err := BuildReply(pkt, MsgNak, opts.ServerIP, Lease{})
			if err != nil {
				return d, err
			}
			d.Reply = raw
			d.MsgType = MsgNak
			return d, nil
		}
		raw, err := BuildReply(pkt, MsgAck, opts.ServerIP, lease)
		if err != nil {
			return d, err
		}
		d.Reply = raw
		d.MsgType = MsgAck
	}
	return d, nil
}
