//go:build darwin

package weft

import (
	"sync"
	"testing"
	"time"
)

// TestEventFilter_KindPrefix covers the kind-matching rules: empty
// prefix list = accept all; non-empty = at least one entry must be
// a prefix of the event's Kind.
func TestEventFilter_KindPrefix(t *testing.T) {
	f := EventFilter{
		KindPrefixes: []string{"vm.", "guest."},
		SeeAll:       true,
	}
	cases := []struct {
		kind string
		ok   bool
	}{
		{"vm.state.running", true},
		{"guest.exec_ready", true},
		{"project.renamed", false},
		{"server.start_attempted", false},
	}
	for _, c := range cases {
		if got := f.accepts(PlatformEvent{Kind: c.kind}); got != c.ok {
			t.Errorf("accepts(%q) = %v, want %v", c.kind, got, c.ok)
		}
	}
}

// TestEventFilter_VisibleACL pins the ACL filter: project_uuid must
// be in the Visible set, or the event must be global (project_uuid
// == ""), to be accepted.
func TestEventFilter_VisibleACL(t *testing.T) {
	f := EventFilter{
		Visible: map[string]struct{}{
			"proj-a": {},
		},
	}
	if !f.accepts(PlatformEvent{ProjectUUID: "proj-a"}) {
		t.Error("event in visible project must pass")
	}
	if f.accepts(PlatformEvent{ProjectUUID: "proj-b"}) {
		t.Error("event in invisible project must be dropped")
	}
	if !f.accepts(PlatformEvent{ProjectUUID: ""}) {
		t.Error("global event (empty project) must reach every subscriber")
	}
}

// TestEventFilter_ProjectNarrowing covers the request-side
// `project` filter: when set, events from other projects are
// dropped on top of the ACL.
func TestEventFilter_ProjectNarrowing(t *testing.T) {
	f := EventFilter{
		SeeAll:  true,
		Project: "proj-a",
	}
	if !f.accepts(PlatformEvent{ProjectUUID: "proj-a"}) {
		t.Error("matching project must pass")
	}
	if f.accepts(PlatformEvent{ProjectUUID: "proj-b"}) {
		t.Error("non-matching project must drop")
	}
}

// TestBusPublishSubscribe round-trips one event through the bus.
func TestBusPublishSubscribe(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe(EventFilter{SeeAll: true})
	defer cancel()
	bus.Publish(PlatformEvent{Kind: "vm.state.running", Subject: "alpine"})
	select {
	case ev := <-ch:
		if ev.Kind != "vm.state.running" || ev.Subject != "alpine" {
			t.Errorf("got %+v", ev)
		}
		if ev.TsUnixNano == 0 {
			t.Error("ts should auto-fill when caller leaves it zero")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}
}

// TestBusDropsWhenSubscriberFull confirms the non-blocking
// publish contract: a stuffed channel drops events rather than
// stalling producers.
func TestBusDropsWhenSubscriberFull(t *testing.T) {
	bus := NewEventBus()
	_, cancel := bus.Subscribe(EventFilter{SeeAll: true})
	defer cancel()
	// Bus channel buffer is 128. Pump 1000 events without reading;
	// the publish loop must complete without blocking.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			bus.Publish(PlatformEvent{Kind: "guest.tick"})
		}
		close(done)
	}()
	select {
	case <-done:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked — drop-on-full contract broken")
	}
}

// TestBusCancelClosesChannel ensures cancel is idempotent and
// the channel closes exactly once.
func TestBusCancelClosesChannel(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe(EventFilter{SeeAll: true})
	if bus.SubscriberCount() != 1 {
		t.Fatalf("want 1 sub, got %d", bus.SubscriberCount())
	}
	cancel()
	cancel() // must not panic (sync.Once)
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}
	if bus.SubscriberCount() != 0 {
		t.Errorf("want 0 subs after cancel, got %d", bus.SubscriberCount())
	}
}

// TestBusConcurrentPublishSubscribe is a smoke test for the
// RWMutex + sync.Once shape. Spins up several publishers + several
// subscribers, every consumer drains a non-trivial number of events.
func TestBusConcurrentPublishSubscribe(t *testing.T) {
	bus := NewEventBus()
	const subs = 4
	const pubsPerWorker = 256
	chans := make([]<-chan PlatformEvent, subs)
	cancels := make([]func(), subs)
	for i := 0; i < subs; i++ {
		chans[i], cancels[i] = bus.Subscribe(EventFilter{SeeAll: true})
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < pubsPerWorker; j++ {
				bus.Publish(PlatformEvent{Kind: "guest.ping"})
			}
		}()
	}
	// Drain each subscriber concurrently so the bus's drop path
	// doesn't kick in for the round-trip count check.
	var rxWG sync.WaitGroup
	got := make([]int, subs)
	for i := 0; i < subs; i++ {
		rxWG.Add(1)
		go func(i int) {
			defer rxWG.Done()
			timeout := time.After(2 * time.Second)
			for {
				select {
				case _, ok := <-chans[i]:
					if !ok {
						return
					}
					got[i]++
				case <-timeout:
					return
				}
			}
		}(i)
	}
	wg.Wait()
	// Give consumers a moment to drain, then cancel.
	time.Sleep(50 * time.Millisecond)
	for _, c := range cancels {
		c()
	}
	rxWG.Wait()
	for i, n := range got {
		if n == 0 {
			t.Errorf("subscriber %d got 0 events", i)
		}
	}
}
