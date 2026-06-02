package firewallpub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

type fakeBus struct {
	mu     sync.Mutex
	events []weft.PlatformEvent
}

func (f *fakeBus) Publish(ev weft.PlatformEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeBus) snapshot() []weft.PlatformEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]weft.PlatformEvent, len(f.events))
	copy(out, f.events)
	return out
}

func TestStatusReceiver_DecodesAndRepublishes(t *testing.T) {
	bus := &fakeBus{}
	rcv, err := NewStatusReceiver(noopSubscribe, bus, silentLog())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload, _ := json.Marshal(pod.FirewallStatus{
		Overall: "Healthy", TableInstalled: true, RulesInstalled: 7,
		PublishedAtUnix: 1700000000,
	})
	rcv.HandleMessage("weft.firewall.vm-42.status", payload)

	ev := bus.snapshot()
	if len(ev) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ev))
	}
	if ev[0].Kind != "firewall.status" {
		t.Errorf("Kind = %q", ev[0].Kind)
	}
	if ev[0].Subject != "vm-42" {
		t.Errorf("Subject = %q", ev[0].Subject)
	}
	if ev[0].Meta["Overall"] != "Healthy" {
		t.Errorf("Meta Overall = %q", ev[0].Meta["Overall"])
	}
	if ev[0].Meta["RulesInstalled"] != "7" {
		t.Errorf("Meta RulesInstalled = %q", ev[0].Meta["RulesInstalled"])
	}
	if ev[0].Meta["TableInstalled"] != "true" {
		t.Errorf("Meta TableInstalled = %q", ev[0].Meta["TableInstalled"])
	}
	if _, has := ev[0].Meta["LastError"]; has {
		t.Errorf("LastError must be omitted when empty: %+v", ev[0].Meta)
	}
}

func TestStatusReceiver_IncludesLastErrorWhenDegraded(t *testing.T) {
	bus := &fakeBus{}
	rcv, _ := NewStatusReceiver(noopSubscribe, bus, silentLog())
	payload, _ := json.Marshal(pod.FirewallStatus{
		Overall: "Degraded", LastError: "netlink EAGAIN",
	})
	rcv.HandleMessage("weft.firewall.vm-9.status", payload)
	if got := bus.snapshot()[0].Meta["LastError"]; got != "netlink EAGAIN" {
		t.Errorf("LastError = %q", got)
	}
}

func TestStatusReceiver_RejectsBadSubject(t *testing.T) {
	bus := &fakeBus{}
	rcv, _ := NewStatusReceiver(noopSubscribe, bus, silentLog())
	for _, subject := range []string{
		"",
		"weft.firewall.vm-1",        // missing .status
		"weft.firewall.status",      // missing UUID
		"weft.firewall..status",     // empty UUID
		"weft.firewall.vm.x.status", // UUID contains dot
		"weft.foo.vm-1.status",      // wrong prefix
	} {
		rcv.HandleMessage(subject, []byte(`{"Overall":"Healthy"}`))
	}
	if got := bus.snapshot(); len(got) != 0 {
		t.Errorf("bad subjects must not publish: %+v", got)
	}
}

func TestStatusReceiver_DropsMalformedJSON(t *testing.T) {
	bus := &fakeBus{}
	rcv, _ := NewStatusReceiver(noopSubscribe, bus, silentLog())
	rcv.HandleMessage("weft.firewall.vm-1.status", []byte("{not json"))
	if got := bus.snapshot(); len(got) != 0 {
		t.Errorf("bad JSON must not publish: %+v", got)
	}
}

func TestStatusReceiver_RunPropagatesCancel(t *testing.T) {
	bus := &fakeBus{}
	called := make(chan struct{})
	sub := func(ctx context.Context, _ string, _ func(string, []byte)) error {
		close(called)
		<-ctx.Done()
		return ctx.Err()
	}
	rcv, _ := NewStatusReceiver(sub, bus, silentLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rcv.Run(ctx) }()
	<-called
	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("err = %v", err)
	}
}

func noopSubscribe(context.Context, string, func(string, []byte)) error { return nil }
