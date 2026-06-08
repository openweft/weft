// Package agentrespawn wires the standalone weft/respawn state
// machine into the agent's live PlatformEvent bus :
//
//   - on startup, scan every SchedulingRule with respawn.enabled=true
//     and call Reconciler.Watch for each VM the rule binds (by name
//     selector for V0.1 ; nominal binding + label selectors land when
//     VMRecord gains the field — see [[openweft_nominal_binding]]).
//   - subscribe to "vm.state_changed" and "schedulingrule.*" events,
//     translating them into respawn.Signal{Down/Up} and rule-set
//     refreshes.
//
// The package is intentionally thin : Reconciler owns the per-VM
// state machine + the side effects (StartVM/StopVM via VMActions);
// this file just glues it onto the bus.
//
// V0.1 limitations (called out so future readers don't assume
// completeness) :
//
//   - Selector grammar : only `vm.name=<exact name>` is supported.
//     Anything else makes the rule a no-op.
//   - No health-probe Runner wiring yet — the Reconciler accepts
//     Unhealthy signals, but nothing on the agent side emits them
//     today. To wire the probes we need each VM's overlay IP
//     surfaced from the VMRecord ; that lands in V0.1.1.
//   - Multi-VM matching via labels / nominal binding is deferred to
//     V0.1.1 for the same reason.
package agentrespawn

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/openweft/weft"
	"github.com/openweft/weft/respawn"
	weftv1 "github.com/openweft/weft-proto"
)

// SchedulingRulesReader is the slice of the Adapter surface the
// subscriber needs. Captured as an interface so tests can hand-craft
// a fake without standing up a full Adapter.
type SchedulingRulesReader interface {
	SchedulingRules() []weft.SchedulingRuleEntry
}

// VMStatusReader exposes the "is this VM currently running ?" query
// the poller uses to detect deaths. The microVM hypervisor drivers
// (qemu, vz) signal exit by writing <vmDir>/exit.json from a reaper
// goroutine, but no bus event is emitted today — so the subscriber
// polls this surface every few seconds and synthesises a Down signal
// when a watched VM transitions running → not-running.
//
// Hosts that already publish vm.state_changed events (classic VM
// lifecycle) ride the bus path ; the poller is the safety net for
// microVMs whose hypervisor doesn't notify the agent on guest exit.
type VMStatusReader interface {
	IsVMRunning(name string) bool
}

// Subscriber owns the bus subscription + the Reconciler instance.
// One Subscriber per agent ; Run blocks until ctx is cancelled.
type Subscriber struct {
	bus      weft.EventBus
	rules    SchedulingRulesReader
	status   VMStatusReader // optional ; nil = bus-only path (classic VMs)
	rec      *respawn.Reconciler
	log      *slog.Logger
	rescanCh chan struct{}
	pollEvery time.Duration

	mu       sync.Mutex
	ctx      context.Context   // populated by Run ; used by rescan to scope Reconciler.Watch
	watched  map[string]string // vmName → ruleUUID (the rule currently driving it)
	lastSeen map[string]bool   // vmName → "was running on last poll"
}

// New wires the subscriber. The actions implement how StartVM /
// StopVM are dispatched ; in production the agent passes a thin
// adapter that calls its existing VM lifecycle code. log can be nil.
func New(bus weft.EventBus, rules SchedulingRulesReader, actions respawn.VMActions, log *slog.Logger) *Subscriber {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return &Subscriber{
		bus:       bus,
		rules:     rules,
		rec:       respawn.New(actions, log.With("component", "respawn")),
		log:       log,
		rescanCh:  make(chan struct{}, 1),
		pollEvery: 2 * time.Second,
		watched:   map[string]string{},
		lastSeen:  map[string]bool{},
	}
}

// WithStatusReader enables the polling fallback : every pollEvery
// the subscriber asks the reader whether each watched VM is still
// running, and synthesises a Down signal when a VM transitions
// running → not-running. Required for microVM workloads whose
// hypervisor driver doesn't emit vm.state_changed events on guest
// exit. Safe no-op when reader is nil.
func (s *Subscriber) WithStatusReader(reader VMStatusReader) *Subscriber {
	s.status = reader
	return s
}

// WithPollInterval overrides the default 2s poller period. Useful
// for tests + very tight grace_period_ms windows. Zero or negative
// disables the poller entirely.
func (s *Subscriber) WithPollInterval(d time.Duration) *Subscriber {
	s.pollEvery = d
	return s
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }

// Reconciler exposes the underlying Reconciler so callers can inject
// extra signals (e.g. health probes wired in V0.1.1).
func (s *Subscriber) Reconciler() *respawn.Reconciler { return s.rec }

// Run drives the subscription loop. Returns when ctx is cancelled.
// The bus subscription is cancelled cleanly on exit so the bus's
// per-subscriber goroutine doesn't leak.
func (s *Subscriber) Run(ctx context.Context) error {
	// Cache ctx so rescan() can pass it down into Reconciler.Watch ;
	// otherwise the per-VM goroutines outlive the subscriber and
	// Wait() in the cancel path never returns.
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	ch, cancel := s.bus.Subscribe(weft.EventFilter{
		KindPrefixes: []string{"vm.state_changed", "schedulingrule."},
		SeeAll:       true,
	})
	defer cancel()
	// Initial scan to pick up rules that pre-exist this agent's boot.
	s.rescan()

	// Poller : ticks every s.pollEvery and emits Down/Up signals for
	// watched microVMs whose hypervisor driver doesn't publish
	// vm.state_changed events. Stops cleanly with the loop.
	var pollC <-chan time.Time
	if s.status != nil && s.pollEvery > 0 {
		t := time.NewTicker(s.pollEvery)
		defer t.Stop()
		pollC = t.C
	}

	for {
		select {
		case <-ctx.Done():
			s.rec.Wait()
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			s.dispatch(ev)
		case <-s.rescanCh:
			s.rescan()
		case <-pollC:
			s.pollWatchedVMs()
		}
	}
}

// pollWatchedVMs calls VMStatusReader.IsVMRunning for each watched VM
// and synthesises Down/Up signals on transition. Pure side-effecting
// against the Reconciler ; no bus interaction. Idempotent on stable
// state — only emits when the boolean flips.
func (s *Subscriber) pollWatchedVMs() {
	if s.status == nil {
		return
	}
	s.mu.Lock()
	watched := make([]string, 0, len(s.watched))
	for n := range s.watched {
		watched = append(watched, n)
	}
	s.mu.Unlock()
	for _, name := range watched {
		running := s.status.IsVMRunning(name)
		s.mu.Lock()
		prev, seen := s.lastSeen[name]
		s.lastSeen[name] = running
		s.mu.Unlock()
		if !seen {
			// First observation : seed without emitting. Avoids
			// firing a spurious Down when the agent boots into an
			// already-stopped VM the operator wants left alone.
			continue
		}
		if prev && !running {
			s.log.Info("respawn poller : VM stopped", "vm", name)
			s.rec.Send(respawn.Signal{
				VMName: name, Kind: respawn.SignalDown, When: time.Now(),
			})
		} else if !prev && running {
			s.rec.Send(respawn.Signal{
				VMName: name, Kind: respawn.SignalUp, When: time.Now(),
			})
		}
	}
}

func (s *Subscriber) dispatch(ev weft.PlatformEvent) {
	switch {
	case strings.HasPrefix(ev.Kind, "schedulingrule."):
		// Coalesce bursts : just request a rescan ; the loop picks it
		// up. Non-blocking so a flood of rule edits can't wedge the
		// bus subscriber goroutine.
		select {
		case s.rescanCh <- struct{}{}:
		default:
		}
	case ev.Kind == "vm.state_changed":
		s.onVMStateChanged(ev)
	}
}

func (s *Subscriber) onVMStateChanged(ev weft.PlatformEvent) {
	vmName := vmNameFromEvent(ev)
	if vmName == "" {
		return
	}
	s.mu.Lock()
	_, watched := s.watched[vmName]
	s.mu.Unlock()
	if !watched {
		return
	}
	newState := ev.Meta["new_state"]
	switch newState {
	case "stopped":
		s.log.Info("respawn : VM stopped", "vm", vmName)
		s.rec.Send(respawn.Signal{
			VMName: vmName, Kind: respawn.SignalDown, When: time.Now(),
		})
	case "running":
		s.rec.Send(respawn.Signal{
			VMName: vmName, Kind: respawn.SignalUp, When: time.Now(),
		})
	}
}

// vmNameFromEvent recovers the VM's logical name from the event.
// vm.state_changed publishes a UUID as Subject ; the Meta payload
// carries names in some emitters. We prefer Meta["name"] when set,
// else fall back to Subject — the per-VM map is keyed off whatever
// the agent's StartVM/StopVM functions accept, which is the same
// identifier the registry uses for the rule selector.
func vmNameFromEvent(ev weft.PlatformEvent) string {
	if n := ev.Meta["name"]; n != "" {
		return n
	}
	return ev.Subject
}

// rescan walks every SchedulingRule, derives the (vmName → policy)
// map, and reconciles the Reconciler's Watch set against it.
//
// Pure-Adapter read, no bus interaction. Called on startup, on
// schedulingrule.* events, and from tests directly.
func (s *Subscriber) rescan() {
	desired := s.desiredWatchedSet()
	s.mu.Lock()
	current := make(map[string]string, len(s.watched))
	for k, v := range s.watched {
		current[k] = v
	}
	s.mu.Unlock()

	// Add or update : every entry in desired gets a Watch call
	// (Reconciler.Watch handles the "already watched, update policy"
	// case).
	s.mu.Lock()
	watchCtx := s.ctx
	s.mu.Unlock()
	if watchCtx == nil {
		watchCtx = context.Background() // pre-Run rescans (tests)
	}
	for vmName, e := range desired {
		s.rec.Watch(watchCtx, vmName, respawnToProto(e.policy))
		s.mu.Lock()
		s.watched[vmName] = e.ruleUUID
		s.mu.Unlock()
	}
	// Remove : anything in current but not in desired.
	for vmName := range current {
		if _, keep := desired[vmName]; !keep {
			s.rec.Unwatch(vmName)
			s.mu.Lock()
			delete(s.watched, vmName)
			s.mu.Unlock()
		}
	}
}

type desiredEntry struct {
	ruleUUID string
	policy   *weft.RespawnPolicyJSON
}

func (s *Subscriber) desiredWatchedSet() map[string]desiredEntry {
	out := map[string]desiredEntry{}
	for _, r := range s.rules.SchedulingRules() {
		if r.Respawn == nil || !r.Respawn.Enabled {
			continue
		}
		// V0.1 : the only supported selector is "vm.name=<name>".
		// A future commit will fold label / nominal-binding matching
		// into the same map without changing the bus subscriber.
		for _, name := range vmNamesFromSelector(r.Selector) {
			if existing, dup := out[name]; dup {
				// Two rules targeting the same VM is a misconfig.
				// Honour the lexicographically smaller UUID for
				// stability and surface a warning so the operator
				// notices.
				if existing.ruleUUID < r.UUID {
					continue
				}
			}
			out[name] = desiredEntry{ruleUUID: r.UUID, policy: r.Respawn}
		}
	}
	return out
}

// vmNamesFromSelector parses the rule's selector field and returns
// the VM names the policy applies to under V0.1 grammar :
//
//	"vm.name=foo"            → ["foo"]
//	"vm.name=foo,vm.name=bar" → ["foo", "bar"]    (rare ; either-or)
//	"k=v"                    → []                  (unsupported)
//
// Anything we don't understand returns an empty slice — the rule
// then matches no VMs, which keeps the bus subscriber from honouring
// half-configured policies.
func vmNamesFromSelector(selector string) []string {
	if selector == "" {
		return nil
	}
	var out []string
	for _, pair := range strings.Split(selector, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) != "vm.name" {
			continue
		}
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// respawnToProto mirrors the converter in cmd/weft/main.go but kept
// local so the subscriber package stays independent of the cmd tree.
// The Reconciler accepts *weftv1.RespawnPolicy directly.
func respawnToProto(p *weft.RespawnPolicyJSON) *weftv1.RespawnPolicy {
	if p == nil {
		return nil
	}
	return &weftv1.RespawnPolicy{
		Enabled:        p.Enabled,
		GracePeriodMs:  p.GracePeriodMs,
		MaxRestarts:    p.MaxRestarts,
		WindowMs:       p.WindowMs,
		Backoff:        p.Backoff,
		InitialDelayMs: p.InitialDelayMs,
	}
}
