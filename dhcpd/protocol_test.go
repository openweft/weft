package dhcpd

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

// buildDiscover assembles a minimal DISCOVER packet for round-trip
// + server-loop tests. xid is the client transaction id, mac the
// chaddr in the first 6 bytes.
func buildDiscover(t *testing.T, xid uint32, mac [6]byte, broadcast bool, requestedIP netip.Addr) []byte {
	t.Helper()
	buf := make([]byte, bootpHeaderSize+4, 300)
	buf[0] = opBootRequest
	buf[1] = htypeEthernet
	buf[2] = 6
	binary.BigEndian.PutUint32(buf[4:8], xid)
	if broadcast {
		binary.BigEndian.PutUint16(buf[10:12], 0x8000)
	}
	copy(buf[28:34], mac[:])
	copy(buf[bootpHeaderSize:bootpHeaderSize+4], magicCookie[:])
	buf = appendOption(buf, optMessageType, []byte{MsgDiscover})
	if requestedIP.IsValid() {
		ip := requestedIP.As4()
		buf = appendOption(buf, optRequestedIP, ip[:])
	}
	// Parameter request list — the server doesn't consult it today
	// but a real client always sends one.
	buf = appendOption(buf, optParameterRequest, []byte{optSubnetMask, optRouter, optDomainNameServer})
	buf = append(buf, optEnd)
	return buf
}

func TestParse_HappyPath(t *testing.T) {
	mac := [6]byte{0x52, 0x54, 0x00, 0x11, 0x22, 0x33}
	raw := buildDiscover(t, 0xdeadbeef, mac, true, netip.MustParseAddr("10.0.0.5"))
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Op != opBootRequest {
		t.Errorf("op = %d, want %d", p.Op, opBootRequest)
	}
	if p.Xid != 0xdeadbeef {
		t.Errorf("xid = %#x, want 0xdeadbeef", p.Xid)
	}
	if p.Flags&0x8000 == 0 {
		t.Errorf("flags = %#x, expected broadcast bit", p.Flags)
	}
	if p.MessageType() != MsgDiscover {
		t.Errorf("MessageType = %d, want %d", p.MessageType(), MsgDiscover)
	}
	if got := p.RequestedIP(); got.String() != "10.0.0.5" {
		t.Errorf("RequestedIP = %v, want 10.0.0.5", got)
	}
	if got := p.MACString(); got != "52:54:00:11:22:33" {
		t.Errorf("MAC = %q, want 52:54:00:11:22:33", got)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want string
	}{
		{"too short", make([]byte, 100), "too short"},
		{"bad cookie", func() []byte {
			b := make([]byte, bootpHeaderSize+4)
			b[bootpHeaderSize] = 0
			return b
		}(), "magic cookie"},
		{"truncated length", func() []byte {
			b := make([]byte, bootpHeaderSize+4)
			copy(b[bootpHeaderSize:bootpHeaderSize+4], magicCookie[:])
			return append(b, 53) // code 53 without length byte
		}(), "truncated"},
		{"truncated value", func() []byte {
			b := make([]byte, bootpHeaderSize+4)
			copy(b[bootpHeaderSize:bootpHeaderSize+4], magicCookie[:])
			return append(b, 53, 4, 1) // claims 4 bytes, supplies 1
		}(), "truncated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.buf)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !contains(err.Error(), c.want) {
				t.Errorf("err = %v, want substring %q", err, c.want)
			}
		})
	}
}

func TestBuildReply_OfferRoundTrip(t *testing.T) {
	mac := [6]byte{0xaa, 0xbb, 0xcc, 0x00, 0x11, 0x22}
	req, err := Parse(buildDiscover(t, 0x12345678, mac, true, netip.Addr{}))
	if err != nil {
		t.Fatalf("Parse discover: %v", err)
	}
	lease := Lease{
		Yiaddr:         netip.MustParseAddr("10.0.0.42"),
		SubnetMaskBits: 24,
		Router:         netip.MustParseAddr("10.0.0.1"),
		DNSServers:     []netip.Addr{netip.MustParseAddr("9.9.9.9"), netip.MustParseAddr("1.1.1.1")},
		Domain:         "openweft.local",
		LeaseTime:      2 * time.Hour,
	}
	raw, err := BuildReply(req, MsgOffer, netip.MustParseAddr("10.0.0.1"), lease)
	if err != nil {
		t.Fatalf("BuildReply: %v", err)
	}

	// Parse back + check every option the server should have set.
	reply, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse reply: %v", err)
	}
	if reply.Op != opBootReply {
		t.Errorf("op = %d, want %d", reply.Op, opBootReply)
	}
	if reply.Xid != 0x12345678 {
		t.Errorf("xid not echoed : got %#x", reply.Xid)
	}
	yi := netip.AddrFrom4(reply.Yiaddr)
	if yi.String() != "10.0.0.42" {
		t.Errorf("yiaddr = %v, want 10.0.0.42", yi)
	}
	if reply.MessageType() != MsgOffer {
		t.Errorf("msg type = %d, want OFFER (%d)", reply.MessageType(), MsgOffer)
	}
	// option 1 subnet mask
	if got := reply.Options[optSubnetMask]; len(got) != 4 || got[0] != 255 || got[1] != 255 || got[2] != 255 || got[3] != 0 {
		t.Errorf("subnet mask = %v, want 255.255.255.0", got)
	}
	// option 3 router
	if got := reply.Options[optRouter]; len(got) != 4 || netip.AddrFrom4([4]byte{got[0], got[1], got[2], got[3]}).String() != "10.0.0.1" {
		t.Errorf("router option = %v", got)
	}
	// option 6 DNS : two servers concatenated.
	if got := reply.Options[optDomainNameServer]; len(got) != 8 {
		t.Errorf("dns option length = %d, want 8", len(got))
	}
	// option 15 domain name
	if got := string(reply.Options[optDomainName]); got != "openweft.local" {
		t.Errorf("domain = %q", got)
	}
	// option 51 lease time
	if got := reply.Options[optLeaseTime]; len(got) != 4 || binary.BigEndian.Uint32(got) != 7200 {
		t.Errorf("lease time bytes = %v, want 7200s", got)
	}
	// option 54 server identifier
	if got := reply.Options[optServerIdentifier]; len(got) != 4 || netip.AddrFrom4([4]byte{got[0], got[1], got[2], got[3]}).String() != "10.0.0.1" {
		t.Errorf("server-id = %v", got)
	}
	// chaddr round-tripped
	if reply.Chaddr[0] != 0xaa || reply.Chaddr[5] != 0x22 {
		t.Errorf("chaddr lost in reply : %v", reply.Chaddr[:6])
	}
}

func TestBuildReply_NakHasNoYiaddr(t *testing.T) {
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	req, _ := Parse(buildDiscover(t, 1, mac, false, netip.Addr{}))
	raw, err := BuildReply(req, MsgNak, netip.MustParseAddr("10.0.0.1"), Lease{})
	if err != nil {
		t.Fatalf("BuildReply NAK: %v", err)
	}
	reply, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse NAK: %v", err)
	}
	if reply.MessageType() != MsgNak {
		t.Errorf("msg type = %d, want NAK", reply.MessageType())
	}
	zero := [4]byte{}
	if reply.Yiaddr != zero {
		t.Errorf("NAK yiaddr = %v, want all-zero", reply.Yiaddr)
	}
	if _, ok := reply.Options[optSubnetMask]; ok {
		t.Error("NAK must not carry subnet mask")
	}
	if _, ok := reply.Options[optLeaseTime]; ok {
		t.Error("NAK must not carry lease time")
	}
}

func TestBuildReply_DefaultLeaseTime(t *testing.T) {
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	req, _ := Parse(buildDiscover(t, 1, mac, false, netip.Addr{}))
	lease := Lease{
		Yiaddr:         netip.MustParseAddr("10.0.0.7"),
		SubnetMaskBits: 24,
	}
	raw, err := BuildReply(req, MsgOffer, netip.MustParseAddr("10.0.0.1"), lease)
	if err != nil {
		t.Fatalf("BuildReply: %v", err)
	}
	reply, _ := Parse(raw)
	got := reply.Options[optLeaseTime]
	if len(got) != 4 || binary.BigEndian.Uint32(got) != 3600 {
		t.Errorf("default lease = %v, want 3600s", got)
	}
}

func TestPrefixToMask(t *testing.T) {
	cases := map[int][4]byte{
		8:  {255, 0, 0, 0},
		16: {255, 255, 0, 0},
		24: {255, 255, 255, 0},
		30: {255, 255, 255, 252},
		32: {255, 255, 255, 255},
	}
	for bits, want := range cases {
		if got := prefixToMask(bits); got != want {
			t.Errorf("prefixToMask(%d) = %v, want %v", bits, got, want)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
