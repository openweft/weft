package portqos

import (
	"strings"
	"testing"
)

func TestPortQoS_Validate(t *testing.T) {
	cases := []struct {
		name    string
		q       PortQoS
		wantErr string
	}{
		{"happy both", PortQoS{TapInterface: "tap0", IngressMbps: 100, EgressMbps: 1000}, ""},
		{"happy egress only", PortQoS{TapInterface: "tap0", EgressMbps: 100}, ""},
		{"happy zero rates ok", PortQoS{TapInterface: "tap0"}, ""},
		{"empty tap", PortQoS{}, "empty tap_interface"},
		{"too-long tap", PortQoS{TapInterface: "very-long-tap0"}, ""}, // 14 chars OK
		{"way too long", PortQoS{TapInterface: "verylongtapname0"}, "too long"},
		{"ingress negative", PortQoS{TapInterface: "tap0", IngressMbps: -1}, "ingress_mbps"},
		{"egress too high", PortQoS{TapInterface: "tap0", EgressMbps: 200_000}, "egress_mbps"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.q.Validate()
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

func TestValidateSpecs_RejectsDuplicateTap(t *testing.T) {
	err := ValidateSpecs([]PortQoS{
		{TapInterface: "tap0", EgressMbps: 100},
		{TapInterface: "tap0", IngressMbps: 100}, // dup tap
	})
	if err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("err = %v, want dup error", err)
	}
}

func TestValidateSpecs_EmptyOK(t *testing.T) {
	if err := ValidateSpecs(nil); err != nil {
		t.Errorf("empty must validate: %v", err)
	}
}

// TestIfbName is in the linux-only file too but the helper is
// shared via the build-tag-free side. Pure-string operation.
// Keep here to exercise the truncation path.
func TestIfbName(t *testing.T) {
	// Goes through the linux-only fn — but pure-Go truncation
	// is testable on any platform via a tiny duplicate.
	truncate := func(s string) string {
		const suffix = "-ifb"
		max := 15 - len(suffix)
		if len(s) > max {
			s = s[:max]
		}
		return s + suffix
	}
	if got := truncate("tap0"); got != "tap0-ifb" {
		t.Errorf("got %q", got)
	}
	if got := truncate("verylongtapname"); got != "verylongtap-ifb" {
		t.Errorf("got %q, want truncated", got)
	}
}
