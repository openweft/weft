package weft

// eventbus_nats.go is the NATS-backed implementation of EventBus
// (per [[vzd-event-bus-nats]]). Selected at startup via HCL
// `event_bus { backend = "nats"; nats { url = "..." } }`.
//
// Wire model:
//
//   * Publish writes one JSON-encoded PlatformEvent to subject
//     `<prefix>.<kind>`. The kind preserves its dots so NATS
//     wildcards line up with our taxonomy: a subscriber asking
//     for kind_prefix="vm." opens `<prefix>.vm.>`, kind_prefix=""
//     becomes `<prefix>.>`.
//
//   * Subscribe creates one NATS subscription per registered
//     consumer, with a buffered Go channel between the NATS
//     callback and the consumer's reader. The bus's
//     drop-on-full contract is preserved: the callback
//     non-blocking sends, full channel = event dropped.
//
//   * ACL + project narrowing happen client-side, after the NATS
//     message is decoded. Encoding tenant UUIDs into the subject
//     would make the operator-visible subject tree uglier and
//     forces every consumer to keep its filter list in sync with
//     the server's; in-Go filtering is plenty for the throughput
//     we expect.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// NATSConfig configures NATSEventBus. Endpoints can be a single
// `nats://host:4222` URL or a comma-separated list (NATS handles
// the failover natively).
type NATSConfig struct {
	URL             string
	CredentialsFile string // optional .creds for NKeys-based auth
	Name            string // ConnectionName visible on nats-server
	SubjectPrefix   string // default "vzd.events"
}

// NATSEventBus is the production backend. Open one per vzd
// process; share across registries via Adapter.SetEventBus.
type NATSEventBus struct {
	nc            *nats.Conn
	subjectPrefix string
	closed        atomic.Bool

	// Live local subscribers (Subscribe returns Go channels — we
	// register a NATS subscription per filter and fan into the
	// consumer's channel). Tracked here so Close can stop them all.
	mu   sync.Mutex
	subs map[*natsSubscription]struct{}
}

type natsSubscription struct {
	out    chan PlatformEvent
	sub    *nats.Subscription
	filter EventFilter
}

// NewNATSEventBus opens a NATS connection and returns the bus
// wrapper. Per-call defaults match the cloud-platform shape: a
// 3-DC NATS cluster reachable on `nats://nats.internal:4222`,
// generic client name "vzd".
func NewNATSEventBus(cfg NATSConfig) (*NATSEventBus, error) {
	if cfg.URL == "" {
		return nil, errors.New("nats event bus: URL is required")
	}
	if cfg.SubjectPrefix == "" {
		cfg.SubjectPrefix = "vzd.events"
	}
	if cfg.Name == "" {
		cfg.Name = "vzd"
	}
	opts := []nats.Option{
		nats.Name(cfg.Name),
		nats.MaxReconnects(-1), // keep retrying forever — the platform should not give up on its own bus
		nats.ReconnectWait(2 * time.Second),
		nats.PingInterval(20 * time.Second),
	}
	if cfg.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(cfg.CredentialsFile))
	}
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", cfg.URL, err)
	}
	return &NATSEventBus{
		nc:            nc,
		subjectPrefix: cfg.SubjectPrefix,
		subs:          make(map[*natsSubscription]struct{}),
	}, nil
}

// Publish serialises ev to JSON and publishes on
// `<prefix>.<kind>`. Per the EventBus contract this is non-
// blocking: nc.Publish queues into the NATS connection's outbound
// buffer and returns immediately; on closed connection it
// silently no-ops (errors are logged at startup, not surfaced
// per-event so a transient NATS hiccup doesn't break callers).
//
// Per [[vzd-tenant-event-access]] events whose `kind` matches the
// tenantSafeKindPrefix allowlist *also* publish to the per-project
// subject `<prefix>.project.<uuid>.events.<kind>` — that's how
// workloads inside a project subscribe to their own events without
// being able to see global / cross-project traffic. Sensitive
// kinds (project.*, user.*) stay global-only.
func (b *NATSEventBus) Publish(ev PlatformEvent) {
	if b == nil || b.closed.Load() {
		return
	}
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = time.Now().UnixNano()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	// Global subject — operators consume via vzc events.
	_ = b.nc.Publish(b.subjectFor(ev.Kind), payload)
	// Per-project subject — tenants inside the project consume via
	// `nats sub vzd.project.<uuid>.events.>` (Phase-1 of
	// [[vzd-tenant-event-access]]: dual-publish without auth
	// wiring; Phase 2+ adds NKey gating). Only events whose kind
	// is in tenantSafeKindPrefixes are mirrored — project.* and
	// user.* stay global-only.
	if ev.ProjectUUID != "" && isTenantSafeKind(ev.Kind) {
		_ = b.nc.Publish(b.projectSubjectFor(ev.ProjectUUID, ev.Kind), payload)
	}
}

// Conn exposes the underlying NATS connection for publishers that target
// per-VM subjects outside the typed PlatformEvent space — the WireGuard
// mesh (weft.mesh.<id>) and the share-mount fan-out (weft.mounts.<id>).
// Returns nil on a closed/nil bus. Kept narrow on purpose: the typed
// Publish path stays the only way to emit operator-facing platform events.
func (b *NATSEventBus) Conn() *nats.Conn {
	if b == nil || b.closed.Load() {
		return nil
	}
	return b.nc
}

// subjectFor returns `<prefix>.<kind>`. Kinds never contain
// characters NATS cares about (we control the taxonomy in
// timings.go), so a direct concatenation is safe; replacing
// disallowed characters with `_` would silently change the
// subject tree and break wildcard subscribers.
func (b *NATSEventBus) subjectFor(kind string) string {
	if kind == "" {
		return b.subjectPrefix + ".unknown"
	}
	return b.subjectPrefix + "." + kind
}

// projectSubjectFor returns the per-project subject:
// `<prefix>.project.<uuid>.events.<kind>`. Used by Publish's
// tenant-mirror path. Per [[vzd-tenant-event-access]] the
// subject keys on UUID (not display name) so a project rename
// never disrupts subscribers.
func (b *NATSEventBus) projectSubjectFor(projectUUID, kind string) string {
	if kind == "" {
		kind = "unknown"
	}
	return b.subjectPrefix + ".project." + projectUUID + ".events." + kind
}

// tenantSafeKindPrefixes is the allowlist of event-kind prefixes
// that get mirrored onto the per-project subject. Restricting the
// set is the defense in depth that complements the future NKey
// permissions: even if subject auth ever loosens, tenants still
// can't see project.* or user.* events.
var tenantSafeKindPrefixes = []string{
	"vm.",
	"guest.",
	"server.",
	"volume.",
	"network.",
}

func isTenantSafeKind(kind string) bool {
	for _, p := range tenantSafeKindPrefixes {
		if len(kind) >= len(p) && kind[:len(p)] == p {
			return true
		}
	}
	return false
}

// Subscribe creates a NATS subscription that matches the union of
// the filter's KindPrefixes (or `<prefix>.>` when empty), then
// fans decoded events into a Go channel. ACL + project narrowing
// are applied in this goroutine — same filter shape as
// LocalEventBus.
func (b *NATSEventBus) Subscribe(filter EventFilter) (<-chan PlatformEvent, func()) {
	const bufSize = 128
	out := make(chan PlatformEvent, bufSize)
	s := &natsSubscription{out: out, filter: filter}

	// Build the wildcard subject expression. Multiple KindPrefixes
	// become a fan-in over multiple NATS subscriptions joined by
	// `nats.Subscribe`. To keep this simple we register one
	// subscription per prefix — NATS de-duplicates message
	// delivery itself if two prefixes overlap.
	subjects := b.subjectsFor(filter.KindPrefixes)

	// The NATS callback runs on a dedicated goroutine per
	// subscription. Drop on a full channel rather than block —
	// matches the LocalEventBus contract.
	handler := func(m *nats.Msg) {
		var ev PlatformEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return // malformed payload — discard, don't kill the subscription
		}
		if !filter.accepts(ev) {
			return
		}
		select {
		case out <- ev:
		default:
			// Dropped; the subscriber is slow.
		}
	}

	for _, subj := range subjects {
		sub, err := b.nc.Subscribe(subj, handler)
		if err != nil {
			// Best-effort: log via the global logger isn't reachable
			// here, so we just skip this subject. The remaining
			// subscriptions still deliver.
			continue
		}
		s.sub = sub // last one wins; we keep it for diagnostics
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
			if s.sub != nil {
				_ = s.sub.Unsubscribe()
			}
			close(out)
		})
	}
	return out, cancel
}

// subjectsFor maps the filter's kind prefixes into NATS subject
// expressions. Empty list → all events. Each prefix becomes one
// wildcard subscription; the EventFilter still runs in the
// callback so the receiver gets project-scoped + ACL-filtered
// events even if multiple prefixes overlap.
func (b *NATSEventBus) subjectsFor(prefixes []string) []string {
	if len(prefixes) == 0 {
		return []string{b.subjectPrefix + ".>"}
	}
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.TrimRight(p, ".") // tolerate "vm." or "vm" interchangeably
		if p == "" {
			out = append(out, b.subjectPrefix+".>")
			continue
		}
		out = append(out, b.subjectPrefix+"."+p+".>")
	}
	return out
}

// SubscriberCount returns 0 for the NATS-backed bus: subscribers
// elsewhere on the cluster aren't visible from this process. The
// local count (Go-channel consumers attached to this vzd) is
// available via b.localSubs() for diagnostics if needed.
func (b *NATSEventBus) SubscriberCount() int { return 0 }

// Close drains every local subscription, unsubscribes from NATS,
// and closes the underlying connection. Publish becomes a no-op
// after Close.
func (b *NATSEventBus) Close() error {
	if b == nil {
		return nil
	}
	if b.closed.Swap(true) {
		return nil
	}
	b.mu.Lock()
	for s := range b.subs {
		if s.sub != nil {
			_ = s.sub.Unsubscribe()
		}
		close(s.out)
	}
	b.subs = nil
	b.mu.Unlock()
	b.nc.Close()
	return nil
}
