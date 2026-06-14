package dhcpd

import (
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// TestDecide_Discover_UnknownMAC verifies the silent-drop path :
// no Reply, no error, and the Source.Resolve hit is observed.
func TestDecide_Discover_UnknownMAC(t *testing.T) {
	var hits int32
	opts := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source: SourceFn(func(mac string) (Lease, bool) {
			atomic.AddInt32(&hits, 1)
			return Lease{}, false
		}),
	}
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	pkt, _ := Parse(buildDiscover(t, 1, mac, true, netip.Addr{}))
	d, err := Decide(pkt, opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Reply != nil {
		t.Errorf("expected drop, got reply len=%d", len(d.Reply))
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("Source.Resolve hits = %d, want 1", got)
	}
}

// TestDecide_Discover_KnownMAC : known MAC → OFFER with the right
// yiaddr.
func TestDecide_Discover_KnownMAC(t *testing.T) {
	opts := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source: SourceFn(func(mac string) (Lease, bool) {
			if mac == "52:54:00:00:00:01" {
				return Lease{
					Yiaddr:         netip.MustParseAddr("10.0.0.42"),
					SubnetMaskBits: 24,
					Router:         netip.MustParseAddr("10.0.0.1"),
					LeaseTime:      time.Hour,
				}, true
			}
			return Lease{}, false
		}),
	}
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	pkt, _ := Parse(buildDiscover(t, 0xcafef00d, mac, true, netip.Addr{}))
	d, err := Decide(pkt, opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.MsgType != MsgOffer {
		t.Errorf("MsgType = %d, want OFFER", d.MsgType)
	}
	reply, err := Parse(d.Reply)
	if err != nil {
		t.Fatalf("Parse reply: %v", err)
	}
	if reply.Op != opBootReply {
		t.Errorf("reply op = %d, want BOOTREPLY", reply.Op)
	}
	if reply.Xid != 0xcafef00d {
		t.Errorf("xid not echoed : %#x", reply.Xid)
	}
	if netip.AddrFrom4(reply.Yiaddr).String() != "10.0.0.42" {
		t.Errorf("yiaddr = %v", netip.AddrFrom4(reply.Yiaddr))
	}
	if reply.MessageType() != MsgOffer {
		t.Errorf("reply msg type = %d, want OFFER", reply.MessageType())
	}
}

// TestDecide_Request_MatchingIP : REQUEST with matching option-50
// → ACK.
func TestDecide_Request_MatchingIP(t *testing.T) {
	opts := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source: SourceFn(func(string) (Lease, bool) {
			return Lease{
				Yiaddr:         netip.MustParseAddr("10.0.0.42"),
				SubnetMaskBits: 24,
			}, true
		}),
	}
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	raw := buildDiscover(t, 1, mac, false, netip.MustParseAddr("10.0.0.42"))
	pkt, _ := Parse(raw)
	// Switch the message type to REQUEST in-place so we don't need
	// a second builder.
	pkt.Options[optMessageType] = []byte{MsgRequest}
	d, err := Decide(pkt, opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.MsgType != MsgAck {
		t.Errorf("MsgType = %d, want ACK (%d)", d.MsgType, MsgAck)
	}
}

// TestDecide_Request_MismatchingIP : REQUEST with option-50 ≠ what
// Source returns → NAK.
func TestDecide_Request_MismatchingIP(t *testing.T) {
	opts := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source: SourceFn(func(string) (Lease, bool) {
			return Lease{
				Yiaddr:         netip.MustParseAddr("10.0.0.42"),
				SubnetMaskBits: 24,
			}, true
		}),
	}
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	raw := buildDiscover(t, 1, mac, false, netip.MustParseAddr("10.0.0.99"))
	pkt, _ := Parse(raw)
	pkt.Options[optMessageType] = []byte{MsgRequest}
	d, err := Decide(pkt, opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.MsgType != MsgNak {
		t.Errorf("MsgType = %d, want NAK (%d)", d.MsgType, MsgNak)
	}
	reply, _ := Parse(d.Reply)
	if reply.MessageType() != MsgNak {
		t.Errorf("reply msg type = %d, want NAK", reply.MessageType())
	}
}

// TestDecide_IgnoresNonRequestOp : a BOOTREPLY (i.e. our own reply
// echoed back through the broadcast domain) must be dropped, not
// answered.
func TestDecide_IgnoresNonRequestOp(t *testing.T) {
	opts := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source:    SourceFn(func(string) (Lease, bool) { return Lease{}, false }),
	}
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	pkt, _ := Parse(buildDiscover(t, 1, mac, false, netip.Addr{}))
	pkt.Op = opBootReply
	d, err := Decide(pkt, opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Reply != nil {
		t.Errorf("expected drop on BOOTREPLY echo, got reply len=%d", len(d.Reply))
	}
}

// TestDecide_IgnoresReleaseInformDecline : RELEASE/DECLINE/INFORM
// are accepted by the spec but the stateless v0 server drops them
// silently.
func TestDecide_IgnoresReleaseInformDecline(t *testing.T) {
	opts := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source: SourceFn(func(string) (Lease, bool) {
			return Lease{Yiaddr: netip.MustParseAddr("10.0.0.42"), SubnetMaskBits: 24}, true
		}),
	}
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	for _, mt := range []byte{MsgDecline, MsgRelease, MsgInform} {
		pkt, _ := Parse(buildDiscover(t, 1, mac, false, netip.Addr{}))
		pkt.Options[optMessageType] = []byte{mt}
		d, err := Decide(pkt, opts)
		if err != nil {
			t.Fatalf("Decide msg=%d: %v", mt, err)
		}
		if d.Reply != nil {
			t.Errorf("msg=%d : expected drop, got reply len=%d", mt, len(d.Reply))
		}
	}
}

// TestDecide_InvalidLeaseFromSource : a Source bug (returns ok=true
// but Lease fails Validate) should error from Decide, not panic
// and not emit a reply.
func TestDecide_InvalidLeaseFromSource(t *testing.T) {
	opts := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source: SourceFn(func(string) (Lease, bool) {
			return Lease{}, true // valid=true, but Yiaddr is zero → validate fails
		}),
	}
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}
	pkt, _ := Parse(buildDiscover(t, 1, mac, false, netip.Addr{}))
	d, err := Decide(pkt, opts)
	if err == nil {
		t.Fatal("expected error for invalid lease from Source")
	}
	if d.Reply != nil {
		t.Errorf("expected nil reply on Source bug, got %d bytes", len(d.Reply))
	}
}
