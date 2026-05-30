//go:build darwin

package weft

// eventbus_nats_unit_test.go covers the pure-Go / no-server-needed
// branches of the NATS bus: constructor validation + defaults,
// subjectsFor mapping, and the closed-state no-ops on Publish /
// SubscriberCount / Close. End-to-end pub/sub against a live NATS
// cluster is an integration concern (no embedded server vendored).

import (
	"testing"
)

func TestNewNATSEventBus_RequiresURL(t *testing.T) {
	if _, err := NewNATSEventBus(NATSConfig{}); err == nil {
		t.Errorf("empty URL should error")
	}
}

func TestNewNATSEventBus_ConnectFailure(t *testing.T) {
	// A syntactically-valid but unreachable URL: nats.Connect fails
	// fast because the port is closed. This exercises the error
	// branch of the constructor (defaults applied, then connect
	// fails).
	_, err := NewNATSEventBus(NATSConfig{
		URL: "nats://127.0.0.1:1", // reserved low port, nothing listens
	})
	if err == nil {
		t.Errorf("connect to dead endpoint should fail")
	}
}

func TestNATSEventBus_SubjectsFor(t *testing.T) {
	b := &NATSEventBus{subjectPrefix: "weft.events"}
	// Empty prefix list → catch-all.
	if got := b.subjectsFor(nil); len(got) != 1 || got[0] != "weft.events.>" {
		t.Errorf("empty prefixes: got %v", got)
	}
	// Trailing dots tolerated; empty prefix in list → catch-all.
	got := b.subjectsFor([]string{"vm.", "guest", ""})
	want := map[string]bool{
		"weft.events.vm.>":    false,
		"weft.events.guest.>": false,
		"weft.events.>":       false,
	}
	for _, s := range got {
		if _, ok := want[s]; !ok {
			t.Errorf("unexpected subject %q in %v", s, got)
		}
		want[s] = true
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("missing subject %q in %v", s, got)
		}
	}
}

func TestNATSEventBus_ProjectSubjectFor_EmptyKind(t *testing.T) {
	b := &NATSEventBus{subjectPrefix: "weft.events"}
	got := b.projectSubjectFor("p-1", "")
	want := "weft.events.project.p-1.events.unknown"
	if got != want {
		t.Errorf("projectSubjectFor empty kind = %q, want %q", got, want)
	}
}

func TestNATSEventBus_ClosedStateNoOps(t *testing.T) {
	// A bus marked closed: Publish is a no-op, Close is idempotent,
	// SubscriberCount returns 0. Constructing one without a live
	// connection is fine because all three short-circuit on the
	// closed flag (and SubscriberCount always returns 0).
	b := &NATSEventBus{subjectPrefix: "weft.events"}
	b.closed.Store(true)

	// Publish on closed bus must not panic (nc is nil).
	b.Publish(PlatformEvent{Kind: "vm.created", ProjectUUID: "p"})

	if got := b.SubscriberCount(); got != 0 {
		t.Errorf("SubscriberCount = %d, want 0", got)
	}

	// Close on an already-closed bus is a no-op returning nil.
	if err := b.Close(); err != nil {
		t.Errorf("Close on closed bus: %v", err)
	}
}

func TestNATSEventBus_NilClose(t *testing.T) {
	var b *NATSEventBus
	if err := b.Close(); err != nil {
		t.Errorf("nil Close should be nil, got %v", err)
	}
}

// TestNATSEventBus_PublishNilReceiver pins the nil-safety guard.
func TestNATSEventBus_PublishNilReceiver(t *testing.T) {
	var b *NATSEventBus
	// Must not panic.
	b.Publish(PlatformEvent{Kind: "x"})
}
