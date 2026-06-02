package floatingipnat

import (
	"strings"
	"testing"
)

func TestNATMapping_Validate(t *testing.T) {
	cases := []struct {
		name    string
		m       NATMapping
		wantErr string
	}{
		{"v4 happy", NATMapping{PublicIP: "203.0.113.5", PrivateIP: "10.0.0.5", VMName: "web"}, ""},
		{"bad public", NATMapping{PublicIP: "not-an-ip", PrivateIP: "10.0.0.5"}, "public_ip"},
		{"bad private", NATMapping{PublicIP: "203.0.113.5", PrivateIP: "not-an-ip"}, "private_ip"},
		{"mismatched family", NATMapping{PublicIP: "203.0.113.5", PrivateIP: "2001:db8::5"}, "same family"},
		{"v6 happy", NATMapping{PublicIP: "2001:db8::5", PrivateIP: "fd00::5"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.m.Validate()
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

func TestValidateMappings_RejectsDuplicatePublicIP(t *testing.T) {
	err := ValidateMappings([]NATMapping{
		{PublicIP: "203.0.113.5", PrivateIP: "10.0.0.5"},
		{PublicIP: "203.0.113.5", PrivateIP: "10.0.0.6"}, // dup
	})
	if err == nil || !strings.Contains(err.Error(), "already mapped") {
		t.Errorf("err = %v, want already-mapped error", err)
	}
}

func TestValidateMappings_EmptyOK(t *testing.T) {
	if err := ValidateMappings(nil); err != nil {
		t.Errorf("empty mappings should validate, got %v", err)
	}
}

func TestFilterToTargetSet_JoinsByVMName(t *testing.T) {
	all := []ControlPlaneMapping{
		{PublicIP: "203.0.113.5", VMName: "vm-a"},
		{PublicIP: "203.0.113.6", VMName: "vm-b"}, // not local
		{PublicIP: "203.0.113.7", VMName: "vm-c"},
	}
	local := map[string]string{
		"vm-a": "10.0.0.5",
		"vm-c": "10.0.0.7",
		"vm-d": "10.0.0.99", // unused — no matching control-plane mapping
	}
	got := FilterToTargetSet(all, local)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 : %+v", len(got), got)
	}
	if got[0].VMName != "vm-a" || got[0].PrivateIP != "10.0.0.5" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].VMName != "vm-c" || got[1].PrivateIP != "10.0.0.7" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestFilterToTargetSet_DropsVMWithEmptyPrivateIP(t *testing.T) {
	all := []ControlPlaneMapping{{PublicIP: "203.0.113.5", VMName: "vm-a"}}
	local := map[string]string{"vm-a": ""} // booted but no IP yet
	if got := FilterToTargetSet(all, local); len(got) != 0 {
		t.Errorf("VM with empty private IP must be filtered out, got %+v", got)
	}
}

func TestFilterToTargetSet_PreservesPublicIPCopy(t *testing.T) {
	// Same VM with two floating IPs — both must survive.
	all := []ControlPlaneMapping{
		{PublicIP: "203.0.113.5", VMName: "vm-a"},
		{PublicIP: "203.0.113.10", VMName: "vm-a"},
	}
	local := map[string]string{"vm-a": "10.0.0.5"}
	got := FilterToTargetSet(all, local)
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}
