package dhcpd

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestLease_Validate(t *testing.T) {
	cases := []struct {
		name    string
		l       Lease
		wantErr string
	}{
		{"happy", Lease{
			Yiaddr:         netip.MustParseAddr("10.0.0.5"),
			SubnetMaskBits: 24,
			Router:         netip.MustParseAddr("10.0.0.1"),
			DNSServers:     []netip.Addr{netip.MustParseAddr("9.9.9.9")},
			Domain:         "openweft.local",
			LeaseTime:      time.Hour,
		}, ""},
		{"no yiaddr", Lease{SubnetMaskBits: 24}, "yiaddr is required"},
		{"v6 yiaddr", Lease{
			Yiaddr:         netip.MustParseAddr("2001:db8::5"),
			SubnetMaskBits: 64,
		}, "DHCPv4"},
		{"bad mask 0", Lease{
			Yiaddr: netip.MustParseAddr("10.0.0.5"),
		}, "subnet_mask_bits"},
		{"bad mask 33", Lease{
			Yiaddr: netip.MustParseAddr("10.0.0.5"), SubnetMaskBits: 33,
		}, "subnet_mask_bits"},
		{"router v6", Lease{
			Yiaddr: netip.MustParseAddr("10.0.0.5"), SubnetMaskBits: 24,
			Router: netip.MustParseAddr("2001:db8::1"),
		}, "router"},
		{"negative lease", Lease{
			Yiaddr: netip.MustParseAddr("10.0.0.5"), SubnetMaskBits: 24,
			LeaseTime: -time.Second,
		}, "lease_time"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.l.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

func TestOptions_Validate(t *testing.T) {
	good := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source:    SourceFn(func(string) (Lease, bool) { return Lease{}, false }),
	}
	if err := good.Validate(); err != nil {
		t.Errorf("happy : %v", err)
	}
	for name, opt := range map[string]Options{
		"empty iface": {ServerIP: netip.MustParseAddr("10.0.0.1"), Source: good.Source},
		"bad serverip": {Interface: "br0", Source: good.Source},
		"nil source":   {Interface: "br0", ServerIP: netip.MustParseAddr("10.0.0.1")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := opt.Validate(); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestStubServer_SimulateRequest(t *testing.T) {
	src := SourceFn(func(mac string) (Lease, bool) {
		if mac == "52:54:00:11:22:33" {
			return Lease{
				Yiaddr:         netip.MustParseAddr("10.0.0.5"),
				SubnetMaskBits: 24,
				Router:         netip.MustParseAddr("10.0.0.1"),
				LeaseTime:      time.Hour,
			}, true
		}
		return Lease{}, false
	})
	s, err := NewStub(Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source:    src,
	})
	if err != nil {
		t.Fatalf("NewStub: %v", err)
	}
	// Known MAC : issued.
	if l, ok := s.SimulateRequest("52:54:00:11:22:33"); !ok || l.Yiaddr.String() != "10.0.0.5" {
		t.Errorf("expected lease for known MAC, got (%+v, %v)", l, ok)
	}
	// Unknown MAC : not issued.
	if _, ok := s.SimulateRequest("aa:bb:cc:dd:ee:ff"); ok {
		t.Error("unknown MAC must not be issued")
	}
	hits := s.Hits()
	if len(hits) != 2 {
		t.Errorf("expected 2 hits, got %d", len(hits))
	}
	if !hits[0].Issued || hits[1].Issued {
		t.Errorf("hit issuance pattern = %v %v, want true,false", hits[0].Issued, hits[1].Issued)
	}
}

func TestStubServer_RunBlocksOnCtx(t *testing.T) {
	s, _ := NewStub(Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source:    SourceFn(func(string) (Lease, bool) { return Lease{}, false }),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Error("Run didn't unblock on cancel")
	}
}
