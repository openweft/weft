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

func TestVMsMatchingSelector(t *testing.T) {
	vms := []VMRef{
		{Name: "loom-1", Properties: map[string]string{"role": "loom", "tier": "prod"}},
		{Name: "loom-2", Properties: map[string]string{"role": "loom", "tier": "dev"}},
		{Name: "api-1", Properties: map[string]string{"role": "api", "tier": "prod"}},
		{Name: "lonely"},
	}
	cases := []struct {
		selector  string
		wantNames []string
	}{
		{"", nil},
		{"vm.name=loom-1", []string{"loom-1"}},
		{"vm.name=loom-1,vm.name=loom-2", []string{"loom-1", "loom-2"}},
		{"role=loom", []string{"loom-1", "loom-2"}},
		{"role=loom,tier=prod", []string{"loom-1"}}, // AND across keys
		{"role=loom,role=api", []string{"loom-1", "loom-2", "api-1"}}, // OR within key
		{"role=loom,vm.name=loom-1", []string{"loom-1"}}, // mixed AND
		{"role=missing", nil},
		{"vm.name=lonely", []string{"lonely"}},
		{"k=", nil}, // empty value, ignored
	}
	for _, tc := range cases {
		got := vmsMatchingSelector(tc.selector, vms)
		gotNames := make([]string, 0, len(got))
		for _, v := range got {
			gotNames = append(gotNames, v.Name)
		}
		// Sort both since selector evaluation order isn't guaranteed.
		sortStrings(gotNames)
		want := append([]string(nil), tc.wantNames...)
		sortStrings(want)
		if !equalStrings(gotNames, want) {
			t.Errorf("selector %q : got %v ; want %v", tc.selector, gotNames, want)
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func TestVMsMatchingSelector_CIGateDefault(t *testing.T) {
	// `deployment.type=ci` VMs are skipped by default even when the
	// selector matches via another key (role=ci-runner is broad).
	vms := []VMRef{
		{Name: "runner-1", Properties: map[string]string{"role": "ci-runner", "deployment.type": "ci"}},
		{Name: "runner-2", Properties: map[string]string{"role": "ci-runner", "deployment.type": "ha"}},
	}
	got := vmsMatchingSelector("role=ci-runner", vms)
	if len(got) != 1 || got[0].Name != "runner-2" {
		t.Errorf("CI gate broken : got %v ; want only runner-2", got)
	}
}

func TestVMsMatchingSelector_CIGateExplicitOverride(t *testing.T) {
	// Operator explicitly opts in to CI respawn by naming
	// `deployment.type=ci` in the selector — gate is suppressed.
	vms := []VMRef{
		{Name: "runner-1", Properties: map[string]string{"role": "ci-runner", "deployment.type": "ci"}},
		{Name: "web-1", Properties: map[string]string{"role": "web", "deployment.type": "ha"}},
	}
	got := vmsMatchingSelector("deployment.type=ci", vms)
	if len(got) != 1 || got[0].Name != "runner-1" {
		t.Errorf("explicit CI override broken : got %v ; want runner-1", got)
	}
}

func TestVMsMatchingSelector_CIGateOROverride(t *testing.T) {
	// OR-clause that includes ci AND ha matches both — operator
	// said "respawn anything HA or CI under this rule".
	vms := []VMRef{
		{Name: "runner", Properties: map[string]string{"deployment.type": "ci"}},
		{Name: "web", Properties: map[string]string{"deployment.type": "ha"}},
		{Name: "stale", Properties: map[string]string{"deployment.type": "stateless"}},
	}
	got := vmsMatchingSelector("deployment.type=ci,deployment.type=ha", vms)
	names := make([]string, 0, len(got))
	for _, v := range got {
		names = append(names, v.Name)
	}
	sortStrings(names)
	want := []string{"runner", "web"}
	if !equalStrings(names, want) {
		t.Errorf("OR-clause override : got %v ; want %v", names, want)
	}
}

func TestVMsMatchingSelector_NoPropertiesNotCI(t *testing.T) {
	// VMs without a deployment.type property are NOT treated as CI
	// (the default is "no opinion, allow respawn").
	vms := []VMRef{
		{Name: "naked", Properties: nil},
		{Name: "labeled", Properties: map[string]string{"role": "loom"}},
	}
	got := vmsMatchingSelector("vm.name=naked,vm.name=labeled", vms)
	if len(got) != 2 {
		t.Errorf("naked VMs got gated : %v", got)
	}
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
