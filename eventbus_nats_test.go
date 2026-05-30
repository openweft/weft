//go:build darwin

package weft

// eventbus_nats_test.go covers the pure-Go bits of the NATS bus
// that don't need a live nats-server: subject-shape rendering
// and the tenant-safe-kind allowlist. Integration tests against
// a real NATS cluster live elsewhere (an embedded server fixture
// is the natural place but kept out of the unit test set so the
// dep stays optional).

import "testing"

func TestIsTenantSafeKind(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		{"vm.state.running", true},
		{"guest.exec_ready", true},
		{"server.start_attempted", true},
		{"volume.created", true},
		{"network.default_sgs_set", true},
		// Sensitive: stay global-only.
		{"project.created", false},
		{"project.renamed", false},
		{"project.member_added", false},
		{"user.created", false},
		// Empty / unknown shapes default to global-only.
		{"", false},
		{"unknown", false},
	}
	for _, c := range cases {
		if got := isTenantSafeKind(c.kind); got != c.want {
			t.Errorf("isTenantSafeKind(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}

func TestNATSSubjectShape(t *testing.T) {
	b := &NATSEventBus{subjectPrefix: "weft.events"}
	cases := []struct {
		kind        string
		wantGlobal  string
		projectUUID string
		wantProject string
	}{
		{
			kind:        "vm.state.running",
			wantGlobal:  "weft.events.vm.state.running",
			projectUUID: "f47ac10b-58cc-4372-a567-0e02b2c3d479",
			wantProject: "weft.events.project.f47ac10b-58cc-4372-a567-0e02b2c3d479.events.vm.state.running",
		},
		{
			kind:       "",
			wantGlobal: "weft.events.unknown",
		},
	}
	for _, c := range cases {
		if got := b.subjectFor(c.kind); got != c.wantGlobal {
			t.Errorf("subjectFor(%q) = %q, want %q", c.kind, got, c.wantGlobal)
		}
		if c.projectUUID != "" {
			if got := b.projectSubjectFor(c.projectUUID, c.kind); got != c.wantProject {
				t.Errorf("projectSubjectFor(%q, %q) = %q, want %q",
					c.projectUUID, c.kind, got, c.wantProject)
			}
		}
	}
}
