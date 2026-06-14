package floatingipl2

import (
	"strings"
	"testing"
)

func TestL2Mapping_Validate(t *testing.T) {
	cases := []struct {
		name    string
		m       L2Mapping
		wantErr string
	}{
		{"happy v4", L2Mapping{PublicIP: "203.0.113.5", NetworkUUID: "n1", VLAN: 100, ParentInterface: "eth0"}, ""},
		{"happy untagged", L2Mapping{PublicIP: "203.0.113.5", NetworkUUID: "n1", VLAN: 0, ParentInterface: "eth0"}, ""},
		{"happy v6", L2Mapping{PublicIP: "2001:db8::5", NetworkUUID: "n1", VLAN: 100, ParentInterface: "eth0"}, ""},
		{"bad ip", L2Mapping{PublicIP: "not-an-ip", NetworkUUID: "n1", ParentInterface: "eth0"}, "public_ip"},
		{"empty network", L2Mapping{PublicIP: "203.0.113.5", ParentInterface: "eth0"}, "network_uuid"},
		{"empty parent", L2Mapping{PublicIP: "203.0.113.5", NetworkUUID: "n1"}, "parent_interface"},
		{"vlan negative", L2Mapping{PublicIP: "203.0.113.5", NetworkUUID: "n1", ParentInterface: "eth0", VLAN: -1}, "vlan out of range"},
		{"vlan too high", L2Mapping{PublicIP: "203.0.113.5", NetworkUUID: "n1", ParentInterface: "eth0", VLAN: 4095}, "vlan out of range"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.m.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("Validate err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

func TestValidateMappings_RejectsDuplicateIP(t *testing.T) {
	err := ValidateMappings([]L2Mapping{
		{PublicIP: "203.0.113.5", NetworkUUID: "n1", ParentInterface: "eth0"},
		{PublicIP: "203.0.113.5", NetworkUUID: "n2", ParentInterface: "eth1"}, // dup IP
	})
	if err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("err = %v, want dup error", err)
	}
}

func TestValidateMappings_AllowsSameNetworkMultipleIPs(t *testing.T) {
	// Same network can carry many FIPs ; the kernel binds them as
	// secondary addresses on the same macvlan.
	if err := ValidateMappings([]L2Mapping{
		{PublicIP: "203.0.113.5", NetworkUUID: "n1", ParentInterface: "eth0", VLAN: 100},
		{PublicIP: "203.0.113.6", NetworkUUID: "n1", ParentInterface: "eth0", VLAN: 100},
		{PublicIP: "203.0.113.7", NetworkUUID: "n1", ParentInterface: "eth0", VLAN: 100},
	}); err != nil {
		t.Errorf("same-network multi-IP must validate: %v", err)
	}
}

func TestValidateMappings_EmptyOK(t *testing.T) {
	if err := ValidateMappings(nil); err != nil {
		t.Errorf("empty input must validate: %v", err)
	}
}
