package weft

// eventbus.go is the in-process publish/subscribe spine for
// weft's PlatformEvent stream. Per [[weft-event-bus]]:
//
//   * Publish is non-blocking — a slow subscriber drops events
//     rather than pushing back on producers. Durable delivery
//     is `VMTimings` (the JSONL log); the bus is the live wire.
//
//   * Subscribe returns a buffered channel + a cancel func.
//     The filter is evaluated server-side so an ACL-restricted
//     subscriber never sees events outside its visible-projects
//     set, even if it spelled a wide kind_prefix.
//
//   * The shape stays identical when we migrate to an etcd-watch
//     backbone (Phase-C of [[etcd-control-plane]]): Publish
//     becomes a `clientv3.Put` on `/weft/events/...`, Subscribe
//     becomes a `clientv3.Watch`. Producer + consumer code
//     never changes — only the backend swaps.

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PlatformEvent is the in-process representation of one event.
// Mirrors the proto's weft.v1.PlatformEvent field-for-field; the
// gRPC handler in cmd/weft marshals between the two.
type PlatformEvent struct {
	TsUnixNano  int64
	Kind        string
	Subject     string
	ProjectUUID string
	Meta        map[string]string
}

// EventFilter is the per-subscription gate the bus applies before
// pushing onto a subscriber's channel.
//
//   * KindPrefixes empty → accept every kind.
//   * KindPrefixes non-empty → at least one entry must be a
//     prefix of the event's Kind.
//   * Visible == nil (and SeeAll false) → match nothing
//     (defensive default; SeeAll must be true for unscoped subs).
//   * Visible non-nil → event's ProjectUUID must be in the set OR
//     the event has no ProjectUUID (global) — global events
//     always reach every subscriber, ACL or not.
//   * SeeAll true → bypass the Visible check entirely.
//   * Project (when set) further narrows the result to one
//     project UUID, on top of Visible.
//   * Subject (when set) narrows further to events whose
//     `Subject` field matches exactly — used by weft / weft-microvm
//     `events --vm <name>` to follow a single VM.
type EventFilter struct {
	KindPrefixes []string
	Visible      map[string]struct{}
	SeeAll       bool
	Project      string
	Subject      string
}

// accepts reports whether the event matches the filter.
func (f EventFilter) accepts(ev PlatformEvent) bool {
	if len(f.KindPrefixes) > 0 {
		matched := false
		for _, p := range f.KindPrefixes {
			if strings.HasPrefix(ev.Kind, p) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if f.Project != "" && ev.ProjectUUID != "" && ev.ProjectUUID != f.Project {
		return false
	}
	if f.Subject != "" && ev.Subject != f.Subject {
		return false
	}
	if f.SeeAll {
		return true
	}
	if ev.ProjectUUID == "" {
		// Global events reach every authenticated subscriber.
		// Project mutations carry their UUID; "global" today is
		// reserved for future use (e.g. node lifecycle).
		return true
	}
	if _, ok := f.Visible[ev.ProjectUUID]; ok {
		return true
	}
	return false
}

// EventBus is the process-wide pub-sub abstraction every producer
// and consumer talks through. Two implementations:
//
//   * LocalEventBus — in-process channels, default for single-host
//     dev. No external dep at runtime.
//   * NATSEventBus  — talks to a NATS cluster on subject
//     `weft.events.<kind>`. Production path, selected via HCL
//     `event_bus { backend = "nats"; nats { url = ... } }`.
//
// Per [[weft-event-bus-nats]] the API is identical across both:
// producer code never has to know which one is wired.
type EventBus interface {
	// Publish delivers ev to every accepting subscriber. MUST be
	// non-blocking — slow consumers drop events rather than push
	// back on producers. Durable delivery is `VMTimings` and (in
	// future) JetStream streams.
	Publish(ev PlatformEvent)
	// Subscribe registers a consumer with the given filter and
	// returns a buffered Go channel + a cancel func. Cancel is
	// idempotent and closes the channel.
	Subscribe(filter EventFilter) (<-chan PlatformEvent, func())
	// SubscriberCount reports the number of live subscribers.
	// NATS-backed implementations return 0 — their subscribers
	// live elsewhere on the cluster and are not visible here.
	SubscriberCount() int
	// Close releases backend resources (NATS connection, etc.).
	// Publish becomes a no-op after Close.
	Close() error
}

// LocalEventBus is the in-process implementation. Methods preserved
// from the previous concrete type so renaming was the only change.
type LocalEventBus struct {
	mu     sync.RWMutex
	subs   map[*subscriber]struct{}
	closed atomic.Bool
}

// subscriber is one open subscription. `out` is the channel the
// consumer reads from; `filter` decides which events reach `out`.
type subscriber struct {
	out    chan PlatformEvent
	filter EventFilter
	// dropped counts events suppressed because `out` was full —
	// surfaced to the consumer via a side-channel `Stats()` call,
	// not in the event stream itself.
	dropped atomic.Uint64
}

// NewEventBus returns an in-process LocalEventBus wrapped in the
// EventBus interface. Used as the dev default; production wires
// NATSEventBus via the cmd/weft factory.
func NewEventBus() EventBus {
	return NewLocalEventBus()
}

// NewLocalEventBus is the explicit constructor for the in-process
// implementation. Tests prefer this over NewEventBus when they
// want to call non-interface methods (e.g. for white-box
// inspection that NATSEventBus wouldn't support).
func NewLocalEventBus() *LocalEventBus {
	return &LocalEventBus{subs: make(map[*subscriber]struct{})}
}

// Publish delivers ev to every accepting subscriber. Non-blocking:
// if a subscriber's channel is full, the event is dropped and
// counted (visible via subscriber.Stats()). Producer code never
// blocks on a slow consumer.
//
// The wall-clock timestamp is filled in when callers leave
// ev.TsUnixNano == 0 so call-sites don't have to import "time".
func (b *LocalEventBus)Publish(ev PlatformEvent) {
	if b == nil || b.closed.Load() {
		return
	}
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = time.Now().UnixNano()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if !s.filter.accepts(ev) {
			continue
		}
		select {
		case s.out <- ev:
		default:
			s.dropped.Add(1)
		}
	}
}

// Subscribe registers a new consumer with the given filter. The
// returned channel delivers matching events; cancel removes the
// subscription and closes the channel. Buffer size defaults to
// 128 — large enough that bursts of timings (boot phase) don't
// drop, small enough that a stuck consumer doesn't bloat memory.
//
// Idempotent: calling cancel more than once is a no-op.
func (b *LocalEventBus)Subscribe(filter EventFilter) (<-chan PlatformEvent, func()) {
	const bufSize = 128
	s := &subscriber{
		out:    make(chan PlatformEvent, bufSize),
		filter: filter,
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
			close(s.out)
		})
	}
	return s.out, cancel
}

// SubscriberCount returns the number of live subscribers — used
// by diagnostics + tests.
func (b *LocalEventBus)SubscriberCount() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Close marks the bus as drained. New Publish calls become no-ops.
// Existing subscriber channels are left open so consumers drain
// in-flight events; their cancel funcs remain valid. Returns nil
// — kept on the signature so the EventBus interface contract
// matches NATSEventBus.Close which can fail on connection
// teardown.
func (b *LocalEventBus) Close() error {
	if b == nil {
		return nil
	}
	b.closed.Store(true)
	return nil
}
