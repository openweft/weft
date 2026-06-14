package weft

import (
	"strings"
	"testing"
)

func TestPickFreeAddress_HappyPath(t *testing.T) {
	got, err := PickFreeAddress("10.0.0.0/29", nil)
	if err != nil {
		t.Fatalf("PickFreeAddress: %v", err)
	}
	// /29 hosts : .0 network, .1-.6 hosts, .7 broadcast.
	// First free = .1.
	if got != "10.0.0.1" {
		t.Errorf("got %s, want 10.0.0.1", got)
	}
}

func TestPickFreeAddress_SkipsExcluded(t *testing.T) {
	got, err := PickFreeAddress("10.0.0.0/29", []string{"10.0.0.1", "10.0.0.2"})
	if err != nil {
		t.Fatalf("PickFreeAddress: %v", err)
	}
	if got != "10.0.0.3" {
		t.Errorf("got %s, want 10.0.0.3", got)
	}
}

func TestPickFreeAddress_PoolExhausted(t *testing.T) {
	excluded := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"}
	_, err := PickFreeAddress("10.0.0.0/29", excluded)
	if err == nil || !strings.Contains(err.Error(), "no free addresses") {
		t.Errorf("expected exhausted err, got %v", err)
	}
}

func TestPickFreeAddress_SkipsNetworkAndBroadcast(t *testing.T) {
	// /30 : .0 network, .1-.2 hosts, .3 broadcast. Picker must
	// never return .0 or .3.
	for i := 0; i < 10; i++ {
		got, err := PickFreeAddress("10.0.0.0/30", nil)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if got == "10.0.0.0" || got == "10.0.0.3" {
			t.Errorf("picker must skip network/broadcast, got %s", got)
		}
	}
}

func TestPickFreeAddress_IPv6Works(t *testing.T) {
	got, err := PickFreeAddress("2001:db8::/64", nil)
	if err != nil {
		t.Fatalf("v6: %v", err)
	}
	// Skip the network address only ; v6 has no broadcast.
	if got != "2001:db8::1" {
		t.Errorf("got %s, want 2001:db8::1", got)
	}
}

func TestPickFreeAddress_Slash32YieldsSingleHost(t *testing.T) {
	// /32 = exactly one IP, no network/broadcast distinction.
	got, err := PickFreeAddress("10.0.0.5/32", nil)
	if err != nil {
		t.Fatalf("/32: %v", err)
	}
	if got != "10.0.0.5" {
		t.Errorf("got %s, want 10.0.0.5", got)
	}
	// Exclude it → exhausted.
	if _, err := PickFreeAddress("10.0.0.5/32", []string{"10.0.0.5"}); err == nil {
		t.Errorf("exclusive /32 must exhaust on exclude")
	}
}

func TestPickFreeAddress_BadCIDR(t *testing.T) {
	if _, err := PickFreeAddress("not-a-cidr", nil); err == nil {
		t.Errorf("expected parse error")
	}
}
