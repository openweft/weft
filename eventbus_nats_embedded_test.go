package weft

import (
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
)

// runEmbeddedNATS starts an in-process nats-server on a random port and
// returns its client URL. The server is shut down via t.Cleanup. This lets
// the NATSEventBus integration paths (connect, publish, subscribe, close)
// run end-to-end without an external broker — the NATS counterpart to the
// embedded etcd harness in storage_etcd_embedded_test.go.
func runEmbeddedNATS(t *testing.T) string {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1 // -1 → OS picks a free port
	srv := natstest.RunServer(&opts)
	if srv == nil {
		t.Fatal("failed to start embedded nats-server")
	}
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server not ready within 5s")
	}
	return srv.ClientURL()
}

// TestNATSEventBus_EmbeddedRoundTrip exercises the full publish/subscribe
// path against a real (embedded) NATS server: NewNATSEventBus connects,
// Subscribe registers a wildcard subscription, Publish fans the event out,
// and the subscriber channel receives it. Closes cleanly at the end.
//
// The subscriber uses SeeAll (operator-style, like `vzc events`) so
// project-scoped events reach it — EventFilter without SeeAll/Visible
// rejects events that carry a ProjectUUID.
func TestNATSEventBus_EmbeddedRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short: brings up an embedded nats-server")
	}
	url := runEmbeddedNATS(t)

	bus, err := NewNATSEventBus(NATSConfig{URL: url, Name: "weft-test"})
	if err != nil {
		t.Fatalf("NewNATSEventBus: %v", err)
	}
	defer func() {
		if err := bus.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	ch, cancel := bus.Subscribe(EventFilter{KindPrefixes: []string{"vm."}, SeeAll: true})
	defer cancel()

	// Give the NATS subscription a moment to register on the server.
	time.Sleep(100 * time.Millisecond)

	bus.Publish(PlatformEvent{Kind: "vm.started", ProjectUUID: "p-1", Subject: "web-01"})

	select {
	case ev := <-ch:
		if ev.Kind != "vm.started" || ev.Subject != "web-01" {
			t.Errorf("received unexpected event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive published event within 3s")
	}
}

// TestNATSEventBus_EmbeddedProjectMirror verifies the tenant-mirror path:
// a tenant-safe kind (vm.*) with a ProjectUUID is published to BOTH the
// global subject and the per-project subject. A SeeAll subscriber receives
// at least one copy.
func TestNATSEventBus_EmbeddedProjectMirror(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short: brings up an embedded nats-server")
	}
	url := runEmbeddedNATS(t)
	bus, err := NewNATSEventBus(NATSConfig{URL: url})
	if err != nil {
		t.Fatalf("NewNATSEventBus: %v", err)
	}
	defer bus.Close()

	ch, cancel := bus.Subscribe(EventFilter{SeeAll: true})
	defer cancel()
	time.Sleep(100 * time.Millisecond)

	bus.Publish(PlatformEvent{Kind: "vm.created", ProjectUUID: "proj-xyz", Subject: "db-1"})

	got := 0
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev := <-ch:
			if ev.Kind == "vm.created" {
				got++
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	if got == 0 {
		t.Fatal("expected at least one vm.created event")
	}
}

// TestNATSEventBus_EmbeddedProjectScopedDelivery proves the per-project
// subject actually carries tenant-safe events: a subscriber whose Visible
// set contains the project UUID (no SeeAll) receives the vm.* event by
// virtue of the dual-publish.
func TestNATSEventBus_EmbeddedProjectScopedDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short: brings up an embedded nats-server")
	}
	url := runEmbeddedNATS(t)
	bus, err := NewNATSEventBus(NATSConfig{URL: url})
	if err != nil {
		t.Fatalf("NewNATSEventBus: %v", err)
	}
	defer bus.Close()

	ch, cancel := bus.Subscribe(EventFilter{
		KindPrefixes: []string{"vm."},
		Visible:      map[string]struct{}{"proj-scoped": {}},
	})
	defer cancel()
	time.Sleep(100 * time.Millisecond)

	bus.Publish(PlatformEvent{Kind: "vm.stopped", ProjectUUID: "proj-scoped", Subject: "app-1"})

	select {
	case ev := <-ch:
		if ev.Kind != "vm.stopped" {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("project-visible subscriber did not receive its event")
	}
}

// TestNATSEventBus_EmbeddedCloseIsIdempotent confirms Close drains cleanly
// and a second Close is a no-op, and Publish after Close is silently dropped.
func TestNATSEventBus_EmbeddedCloseIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short: brings up an embedded nats-server")
	}
	url := runEmbeddedNATS(t)
	bus, err := NewNATSEventBus(NATSConfig{URL: url})
	if err != nil {
		t.Fatalf("NewNATSEventBus: %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Errorf("second Close should be no-op: %v", err)
	}
	// Publish after Close must not panic.
	bus.Publish(PlatformEvent{Kind: "vm.started"})
}
