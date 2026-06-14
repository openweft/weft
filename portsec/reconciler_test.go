package portsec

import (
	"strings"
	"testing"
)

func TestAntispoofRule_Validate(t *testing.T) {
	cases := []struct {
		name    string
		r       AntispoofRule
		wantErr string
	}{
		{"happy", AntispoofRule{TapInterface: "tap0", MAC: "52:54:00:01:02:03", IPs: []string{"10.0.0.5"}}, ""},
		{"mac dash form", AntispoofRule{TapInterface: "tap0", MAC: "52-54-00-01-02-03"}, ""},
		{"v6 ip", AntispoofRule{TapInterface: "tap0", MAC: "52:54:00:01:02:03", IPs: []string{"2001:db8::5"}}, ""},
		{"no IPs OK (boot pre-DHCP)", AntispoofRule{TapInterface: "tap0", MAC: "52:54:00:01:02:03"}, ""},

		{"empty tap", AntispoofRule{MAC: "52:54:00:01:02:03"}, "empty tap_interface"},
		{"too-long tap", AntispoofRule{TapInterface: "verylongtapname0", MAC: "52:54:00:01:02:03"}, "too long"},
		{"empty mac", AntispoofRule{TapInterface: "tap0"}, "empty mac"},
		{"bad mac", AntispoofRule{TapInterface: "tap0", MAC: "not-a-mac"}, "malformed"},
		{"bad ip", AntispoofRule{TapInterface: "tap0", MAC: "52:54:00:01:02:03", IPs: []string{"not-an-ip"}}, "ip[0]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.r.Validate()
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

func TestValidateRules_RejectsDuplicateTap(t *testing.T) {
	err := ValidateRules([]AntispoofRule{
		{TapInterface: "tap0", MAC: "52:54:00:01:02:03"},
		{TapInterface: "tap0", MAC: "52:54:00:01:02:04"}, // dup tap
	})
	if err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("err = %v, want dup tap error", err)
	}
}

func TestValidateRules_RejectsDuplicateMAC(t *testing.T) {
	// Two ports on different taps but the SAME MAC = clear
	// configuration error (typical when an operator clones a Port
	// without rotating MAC). The reconciler refuses ; the caller
	// must fix one of the two.
	err := ValidateRules([]AntispoofRule{
		{TapInterface: "tap0", MAC: "52:54:00:01:02:03"},
		{TapInterface: "tap1", MAC: "52:54:00:01:02:03"},
	})
	if err == nil || !strings.Contains(err.Error(), "cross-tenant collision") {
		t.Errorf("err = %v, want dup mac error", err)
	}
}

func TestValidateRules_EmptyOK(t *testing.T) {
	if err := ValidateRules(nil); err != nil {
		t.Errorf("empty input must validate: %v", err)
	}
}

func TestSortRules_StableByTap(t *testing.T) {
	got := SortRules([]AntispoofRule{
		{TapInterface: "tap2", MAC: "52:54:00:00:00:02"},
		{TapInterface: "tap0", MAC: "52:54:00:00:00:00"},
		{TapInterface: "tap1", MAC: "52:54:00:00:00:01"},
	})
	want := []string{"tap0", "tap1", "tap2"}
	for i, r := range got {
		if r.TapInterface != want[i] {
			t.Errorf("got[%d].TapInterface = %s, want %s", i, r.TapInterface, want[i])
		}
	}
}
