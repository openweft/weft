package agentrespawn

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openweft/weft"
)

// ---- fakes ---------------------------------------------------------

type fakeBus struct {
	mu   sync.Mutex
	subs []chan weft.PlatformEvent
}

func (b *fakeBus) Publish(ev weft.PlatformEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (b *fakeBus) Subscribe(_ weft.EventFilter) (<-chan weft.PlatformEvent, func()) {
	ch := make(chan weft.PlatformEvent, 16)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, c := range b.subs {
			if c == ch {
				b.subs = append(b.subs[:i], b.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}
}

func (b *fakeBus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (b *fakeBus) Close() error { return nil }

type fakeRules struct {
	mu    sync.Mutex
	rules []weft.SchedulingRuleEntry
}

func (r *fakeRules) SchedulingRules() []weft.SchedulingRuleEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]weft.SchedulingRuleEntry, len(r.rules))
	copy(out, r.rules)
	return out
}

func (r *fakeRules) set(rules ...weft.SchedulingRuleEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append([]weft.SchedulingRuleEntry(nil), rules...)
}

type fakeActions struct {
	starts int64
	stops  int64
	wait   time.Duration
}

func (f *fakeActions) StartVM(_ context.Context, _ string) error {
	if f.wait > 0 {
		time.Sleep(f.wait)
	}
	atomic.AddInt64(&f.starts, 1)
	return nil
}
func (f *fakeActions) StopVM(_ context.Context, _ string) error {
	atomic.AddInt64(&f.stops, 1)
	return nil
}

// ---- tests ---------------------------------------------------------

func TestVMNamesFromSelector(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"vm.name=foo", []string{"foo"}},
		{"vm.name=foo,vm.name=bar", []string{"foo", "bar"}},
		{" vm.name = foo ", []string{"foo"}},
		{"role=loom", nil},
		{"vm.name=foo,role=loom", []string{"foo"}},
		{"vm.name=", nil},
	}
	for _, tc := range cases {
		got := vmNamesFromSelector(tc.in)
		if !equalStrings(got, tc.want) {
			t.Errorf("selector %q: got %v ; want %v", tc.in, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSubscriber_RestartsVMOnStopEvent(t *testing.T) {
	bus := &fakeBus{}
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "r1", Name: "loom", Selector: "vm.name=vm-loom",
		Respawn: &weft.RespawnPolicyJSON{Enabled: true, MaxRestarts: 5, WindowMs: 60000},
	})
	acts := &fakeActions{}
	sub := New(bus, rules, acts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = sub.Run(ctx); close(done) }()

	waitFor(t, time.Second, func() bool { return bus.SubscriberCount() == 1 })

	bus.Publish(weft.PlatformEvent{
		Kind: "vm.state_changed", Subject: "vm-loom",
		Meta: map[string]string{"new_state": "stopped"},
	})

	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&acts.starts) >= 1 })

	if got := atomic.LoadInt64(&acts.starts); got != 1 {
		t.Errorf("StartVM fired %d times ; want 1", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSubscriber_IgnoresUnwatchedVM(t *testing.T) {
	bus := &fakeBus{}
	rules := &fakeRules{} // no rules at all
	acts := &fakeActions{}
	sub := New(bus, rules, acts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)
	waitFor(t, time.Second, func() bool { return bus.SubscriberCount() == 1 })

	bus.Publish(weft.PlatformEvent{
		Kind: "vm.state_changed", Subject: "vm-ghost",
		Meta: map[string]string{"new_state": "stopped"},
	})
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&acts.starts); got != 0 {
		t.Errorf("Start fired for unwatched VM : %d calls", got)
	}
}

func TestSubscriber_RescansOnRuleEvent(t *testing.T) {
	bus := &fakeBus{}
	rules := &fakeRules{}
	acts := &fakeActions{}
	sub := New(bus, rules, acts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)
	waitFor(t, time.Second, func() bool { return bus.SubscriberCount() == 1 })

	// Initially the VM is not watched ; a stop event should be ignored.
	bus.Publish(weft.PlatformEvent{
		Kind: "vm.state_changed", Subject: "vm-late",
		Meta: map[string]string{"new_state": "stopped"},
	})
	time.Sleep(30 * time.Millisecond)

	// Now an operator creates the rule. The bus publishes a
	// schedulingrule.created event, which triggers a rescan.
	rules.set(weft.SchedulingRuleEntry{
		UUID: "r2", Name: "late", Selector: "vm.name=vm-late",
		Respawn: &weft.RespawnPolicyJSON{Enabled: true, MaxRestarts: 5, WindowMs: 60000},
	})
	bus.Publish(weft.PlatformEvent{Kind: "schedulingrule.created", Subject: "r2"})

	// After the rescan settles, another stop event should fire Start.
	waitFor(t, time.Second, func() bool {
		bus.Publish(weft.PlatformEvent{
			Kind: "vm.state_changed", Subject: "vm-late",
			Meta: map[string]string{"new_state": "stopped"},
		})
		return atomic.LoadInt64(&acts.starts) >= 1
	})
}

func TestSubscriber_StopsCleanlyOnCancel(t *testing.T) {
	bus := &fakeBus{}
	rules := &fakeRules{}
	acts := &fakeActions{}
	sub := New(bus, rules, acts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = sub.Run(ctx); close(done) }()
	waitFor(t, time.Second, func() bool { return bus.SubscriberCount() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit on cancel")
	}
	if bus.SubscriberCount() != 0 {
		t.Errorf("subscriber leaked : %d remaining", bus.SubscriberCount())
	}
}

func TestSubscriber_DisabledRuleIsIgnored(t *testing.T) {
	bus := &fakeBus{}
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "r3", Name: "off", Selector: "vm.name=vm-off",
		Respawn: &weft.RespawnPolicyJSON{Enabled: false},
	})
	acts := &fakeActions{}
	sub := New(bus, rules, acts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)
	waitFor(t, time.Second, func() bool { return bus.SubscriberCount() == 1 })

	bus.Publish(weft.PlatformEvent{
		Kind: "vm.state_changed", Subject: "vm-off",
		Meta: map[string]string{"new_state": "stopped"},
	})
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&acts.starts); got != 0 {
		t.Errorf("disabled rule still respawned : %d", got)
	}
}

func TestSubscriber_RuleDeletionUnwatches(t *testing.T) {
	bus := &fakeBus{}
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "r4", Name: "tmp", Selector: "vm.name=vm-tmp",
		Respawn: &weft.RespawnPolicyJSON{Enabled: true, MaxRestarts: 5, WindowMs: 60000},
	})
	acts := &fakeActions{}
	sub := New(bus, rules, acts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)
	waitFor(t, time.Second, func() bool { return bus.SubscriberCount() == 1 })

	// Confirm it's watched : one stop → one start.
	bus.Publish(weft.PlatformEvent{
		Kind: "vm.state_changed", Subject: "vm-tmp",
		Meta: map[string]string{"new_state": "stopped"},
	})
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&acts.starts) >= 1 })

	// Remove the rule + signal the change.
	rules.set()
	bus.Publish(weft.PlatformEvent{Kind: "schedulingrule.deleted", Subject: "r4"})
	time.Sleep(80 * time.Millisecond)

	// Subsequent stop is ignored.
	starts := atomic.LoadInt64(&acts.starts)
	bus.Publish(weft.PlatformEvent{
		Kind: "vm.state_changed", Subject: "vm-tmp",
		Meta: map[string]string{"new_state": "stopped"},
	})
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&acts.starts); got != starts {
		t.Errorf("rule deletion did not unwatch : starts went %d → %d", starts, got)
	}
}

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never reached")
}
