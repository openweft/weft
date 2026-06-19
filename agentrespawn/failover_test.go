package agentrespawn

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openweft/weft"
	"github.com/openweft/weft/etcdcoord"
)

// fakeCoord captures claim calls for assertions + returns a canned
// orphan list. Standalone (no etcd dial) so failover unit tests run
// in milliseconds.
type fakeCoord struct {
	mu         sync.Mutex
	localID    string
	orphans    map[string][]VMRef // hostUUID → orphans living there
	claimed    []string           // VM UUIDs claimed, in order
	claimErr   error
	markedDown []string // host UUIDs that consumeHostEvents asked us to mark Down
}

func (c *fakeCoord) LocalHostUUID() string { return c.localID }
func (c *fakeCoord) VMsOnHost(h string) []VMRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]VMRef, len(c.orphans[h]))
	copy(out, c.orphans[h])
	return out
}
func (c *fakeCoord) ListAllVMs() []VMRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []VMRef
	for _, vms := range c.orphans {
		out = append(out, vms...)
	}
	return out
}
func (c *fakeCoord) ClaimVM(uuid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimErr != nil {
		return c.claimErr
	}
	c.claimed = append(c.claimed, uuid)
	return nil
}
func (c *fakeCoord) MarkHostDown(uuid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markedDown = append(c.markedDown, uuid)
	return nil
}
func (c *fakeCoord) claimCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.claimed)
}

// startSubscriberWithCoord wires up a Subscriber + drives its Run
// loop. Returns the bus, the host-event channel the test can push
// HostEvents onto, and a cancel func.
func startSubscriberWithCoord(t *testing.T, rules *fakeRules, acts *fakeActions, coord *fakeCoord) (*fakeBus, chan etcdcoord.HostEvent, context.CancelFunc) {
	t.Helper()
	bus := &fakeBus{}
	events := make(chan etcdcoord.HostEvent, 8)
	// etcdCli=nil → executeClaim takes the no-etcd shortcut path,
	// dodging an embed.Etcd boot for unit tests.
	sub := New(bus, rules, acts, nil).WithCoordinator(coord, events, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sub.Run(ctx) }()
	waitFor(t, time.Second, func() bool { return bus.SubscriberCount() == 1 })
	_ = sub
	return bus, events, cancel
}

func TestFailover_ClaimsOrphansOfDeadHost(t *testing.T) {
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "rule-loom", Name: "loom",
		Selector: "vm.name=vm-loom-1,vm.name=vm-loom-2",
		Respawn:  &weft.RespawnPolicyJSON{Enabled: true, MaxRestarts: 5, WindowMs: 60000},
	})
	acts := &fakeActions{}
	coord := &fakeCoord{
		localID: "host-alive",
		orphans: map[string][]VMRef{
			"host-dead": {
				{UUID: "u1", Name: "vm-loom-1"},
				{UUID: "u2", Name: "vm-loom-2"},
				{UUID: "u3", Name: "vm-not-managed"}, // not in rule selector
			},
		},
	}
	_, events, cancel := startSubscriberWithCoord(t, rules, acts, coord)
	defer cancel()

	events <- etcdcoord.HostEvent{Kind: etcdcoord.HostDown, HostUUID: "host-dead"}

	waitFor(t, 3*time.Second, func() bool { return coord.claimCount() >= 2 })
	coord.mu.Lock()
	claimed := append([]string(nil), coord.claimed...)
	coord.mu.Unlock()
	if len(claimed) != 2 {
		t.Errorf("claimed %d VMs ; want 2 (u1+u2) ; got %v", len(claimed), claimed)
	}
	for _, c := range claimed {
		if c == "u3" {
			t.Error("claimed u3 (not in rule selector)")
		}
	}
	// Down signals fired → Reconciler executes Start.
	waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt64(&acts.starts) >= 2 })
}

func TestFailover_NeverClaimsOwnHost(t *testing.T) {
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "rule", Selector: "vm.name=vm-self",
		Respawn: &weft.RespawnPolicyJSON{Enabled: true, MaxRestarts: 5, WindowMs: 60000},
	})
	acts := &fakeActions{}
	coord := &fakeCoord{
		localID: "host-self",
		orphans: map[string][]VMRef{"host-self": {{UUID: "u1", Name: "vm-self"}}},
	}
	_, events, cancel := startSubscriberWithCoord(t, rules, acts, coord)
	defer cancel()
	events <- etcdcoord.HostEvent{Kind: etcdcoord.HostDown, HostUUID: "host-self"}

	time.Sleep(300 * time.Millisecond)
	if got := coord.claimCount(); got != 0 {
		t.Errorf("self-host claim count = %d ; want 0", got)
	}
}

func TestFailover_RuleWithDisabledRespawnSkipped(t *testing.T) {
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "rule", Selector: "vm.name=vm-x",
		Respawn: &weft.RespawnPolicyJSON{Enabled: false}, // disabled
	})
	acts := &fakeActions{}
	coord := &fakeCoord{
		localID: "host-alive",
		orphans: map[string][]VMRef{"host-dead": {{UUID: "u1", Name: "vm-x"}}},
	}
	_, events, cancel := startSubscriberWithCoord(t, rules, acts, coord)
	defer cancel()
	events <- etcdcoord.HostEvent{Kind: etcdcoord.HostDown, HostUUID: "host-dead"}

	time.Sleep(300 * time.Millisecond)
	if got := coord.claimCount(); got != 0 {
		t.Errorf("disabled-respawn claim count = %d ; want 0", got)
	}
}

func TestFailover_HostUpIsIgnored(t *testing.T) {
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "rule", Selector: "vm.name=vm-x",
		Respawn: &weft.RespawnPolicyJSON{Enabled: true, MaxRestarts: 5, WindowMs: 60000},
	})
	acts := &fakeActions{}
	coord := &fakeCoord{
		localID: "host-alive",
		orphans: map[string][]VMRef{"host-other": {{UUID: "u1", Name: "vm-x"}}},
	}
	_, events, cancel := startSubscriberWithCoord(t, rules, acts, coord)
	defer cancel()
	events <- etcdcoord.HostEvent{Kind: etcdcoord.HostUp, HostUUID: "host-other"}

	time.Sleep(200 * time.Millisecond)
	if got := coord.claimCount(); got != 0 {
		t.Errorf("HostUp triggered claim ; count=%d", got)
	}
}

func TestFailover_NoOrphansForDeadHostNoOp(t *testing.T) {
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "rule", Selector: "vm.name=vm-x",
		Respawn: &weft.RespawnPolicyJSON{Enabled: true, MaxRestarts: 5, WindowMs: 60000},
	})
	acts := &fakeActions{}
	coord := &fakeCoord{
		localID: "host-alive",
		orphans: map[string][]VMRef{}, // empty
	}
	_, events, cancel := startSubscriberWithCoord(t, rules, acts, coord)
	defer cancel()
	events <- etcdcoord.HostEvent{Kind: etcdcoord.HostDown, HostUUID: "host-empty"}

	time.Sleep(200 * time.Millisecond)
	if got := coord.claimCount(); got != 0 {
		t.Errorf("no-orphan host triggered claim ; count=%d", got)
	}
}

// TestFailover_MarksHostDownBeforeClaim covers the registry-side
// side-effect added so `weft host ls` reflects the etcd watcher's
// liveness verdict immediately. Every HostDown event must propagate
// MarkHostDown on the coordinator BEFORE the orphan claim spawns —
// otherwise an idle host whose dispatch session never opened stays
// "active" forever.
func TestFailover_MarksHostDownBeforeClaim(t *testing.T) {
	rules := &fakeRules{}
	acts := &fakeActions{}
	coord := &fakeCoord{
		localID: "host-alive",
		orphans: map[string][]VMRef{"host-dead": {{UUID: "u-1", Name: "vm-1"}}},
	}
	_, events, cancel := startSubscriberWithCoord(t, rules, acts, coord)
	defer cancel()
	events <- etcdcoord.HostEvent{Kind: etcdcoord.HostDown, HostUUID: "host-dead"}

	time.Sleep(200 * time.Millisecond)
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if len(coord.markedDown) != 1 || coord.markedDown[0] != "host-dead" {
		t.Errorf("markedDown = %v, want [host-dead]", coord.markedDown)
	}
}

// TestFailover_HostUpDoesntMarkDown : the opposite signal. HostUp
// events arrive when a previously-dead host comes back ; we must
// NOT flip its registry state to Down.
func TestFailover_HostUpDoesntMarkDown(t *testing.T) {
	rules := &fakeRules{}
	acts := &fakeActions{}
	coord := &fakeCoord{localID: "host-alive"}
	_, events, cancel := startSubscriberWithCoord(t, rules, acts, coord)
	defer cancel()
	events <- etcdcoord.HostEvent{Kind: etcdcoord.HostUp, HostUUID: "host-back"}

	time.Sleep(200 * time.Millisecond)
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if len(coord.markedDown) != 0 {
		t.Errorf("HostUp event marked %v Down — should be a no-op", coord.markedDown)
	}
}

func TestFailover_ClaimErrorDoesntBlockOtherClaims(t *testing.T) {
	rules := &fakeRules{}
	rules.set(weft.SchedulingRuleEntry{
		UUID: "rule",
		// Two VMs targeted ; first claim fails but second should still
		// proceed (per-VM error isolation in executeClaim).
		Selector: "vm.name=vm-a,vm.name=vm-b",
		Respawn:  &weft.RespawnPolicyJSON{Enabled: true, MaxRestarts: 5, WindowMs: 60000},
	})
	acts := &fakeActions{}
	coord := &fakeCoord{
		localID: "host-alive",
		orphans: map[string][]VMRef{"host-dead": {
			{UUID: "u-a", Name: "vm-a"},
			{UUID: "u-b", Name: "vm-b"},
		}},
		claimErr: nil, // we set it via a custom impl below
	}
	// Use a wrapper that fails only the first claim.
	wrap := &flakyCoord{base: coord, failFirst: true}
	bus := &fakeBus{}
	events := make(chan etcdcoord.HostEvent, 1)
	sub := New(bus, rules, acts, nil).WithCoordinator(wrap, events, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	waitFor(t, time.Second, func() bool { return bus.SubscriberCount() == 1 })

	events <- etcdcoord.HostEvent{Kind: etcdcoord.HostDown, HostUUID: "host-dead"}

	waitFor(t, 2*time.Second, func() bool { return coord.claimCount() >= 1 })
	if got := coord.claimCount(); got != 1 {
		t.Errorf("expected 1 successful claim (second), got %d", got)
	}
}

// flakyCoord wraps a fakeCoord but fails the very first ClaimVM call.
// Verifies per-VM error isolation in the claim loop.
type flakyCoord struct {
	base      *fakeCoord
	failFirst bool
	mu        sync.Mutex
	calls     int
}

func (f *flakyCoord) LocalHostUUID() string         { return f.base.LocalHostUUID() }
func (f *flakyCoord) VMsOnHost(h string) []VMRef    { return f.base.VMsOnHost(h) }
func (f *flakyCoord) ListAllVMs() []VMRef           { return f.base.ListAllVMs() }
func (f *flakyCoord) MarkHostDown(uuid string) error { return f.base.MarkHostDown(uuid) }
func (f *flakyCoord) ClaimVM(uuid string) error {
	f.mu.Lock()
	first := f.failFirst && f.calls == 0
	f.calls++
	f.mu.Unlock()
	if first {
		return errFailFirst
	}
	return f.base.ClaimVM(uuid)
}

var errFailFirst = errFlaky("flaky : first call fails")

type errFlaky string

func (e errFlaky) Error() string { return string(e) }
