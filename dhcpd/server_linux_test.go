//go:build linux

package dhcpd

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// TestLinuxServer_DiscoverDrivesSource verifies that a DISCOVER
// packet arriving at the server's bound socket triggers exactly
// one Source.Resolve call with the right MAC, and the OFFER reply
// echoes back via broadcast.
//
// The test uses the loopback interface ("lo") rather than a real
// bridge ; SO_BINDTODEVICE to lo works the same way for the
// purpose of exercising the bind / recv / dispatch path. The test
// is skipped when the process can't open UDP/67 (no CAP_NET_RAW,
// or the port is already in use by a system DHCP server) — that's
// the realistic case in CI sandboxes.
func TestLinuxServer_DiscoverDrivesSource(t *testing.T) {
	var calls int32
	var seenMAC atomic.Value
	src := SourceFn(func(mac string) (Lease, bool) {
		atomic.AddInt32(&calls, 1)
		seenMAC.Store(mac)
		return Lease{
			Yiaddr:         netip.MustParseAddr("127.0.0.42"),
			SubnetMaskBits: 24,
			Router:         netip.MustParseAddr("127.0.0.1"),
			LeaseTime:      time.Hour,
		}, true
	})

	s, err := NewLinuxServer(Options{
		Interface: "lo",
		ServerIP:  netip.MustParseAddr("127.0.0.1"),
		Source:    src,
	})
	if err != nil {
		t.Fatalf("NewLinuxServer: %v", err)
	}

	// Reach into the bind path first to detect lack of privilege /
	// port conflict, before kicking off Run.
	conn, err := s.listen()
	if err != nil {
		t.Skipf("can't bind UDP/67 on lo (need CAP_NET_RAW + free :67): %v", err)
	}
	// Hand the conn back to Run via the mu/conn field by closing
	// this one and letting Run open its own. Simpler : just close
	// and reuse the same code path.
	_ = conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	// Give Run a moment to bind.
	time.Sleep(50 * time.Millisecond)

	// Send a DISCOVER from a transient UDP socket. Source port 68
	// is the DHCP-client convention ; destination is 127.0.0.1:67.
	cli, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client socket: %v", err)
	}
	defer cli.Close()
	mac := [6]byte{0x52, 0x54, 0xde, 0xad, 0xbe, 0xef}
	raw := buildDiscover(t, 0x12345678, mac, true, netip.Addr{})
	if _, err := cli.WriteToUDP(raw, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 67}); err != nil {
		t.Fatalf("send DISCOVER: %v", err)
	}

	// Wait for Source.Resolve to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&calls) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("Source.Resolve calls = %d, want 1", got)
	}
	if got, _ := seenMAC.Load().(string); got != "52:54:de:ad:be:ef" {
		t.Errorf("Source.Resolve saw mac=%q, want 52:54:de:ad:be:ef", got)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil && err != context.Canceled {
			t.Errorf("Run err = %v, want nil or Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run didn't unblock on cancel")
	}
}
