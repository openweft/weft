package firewallpub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

// StatusReceiver is the host-side counterpart of pkg/firewallstatus
// in weft-microvm-agent : it subscribes to every per-VM
// "weft.firewall.<vm-uuid>.status" subject via a NATS wildcard,
// decodes [[pod.FirewallStatus]], and re-emits each decoded status
// as a [[weft.PlatformEvent]] of kind "firewall.status" on the
// existing event bus.
//
// Why synthesize a PlatformEvent rather than expose a new transport ?
// Two reasons :
//
//  1. The webui already streams platform events via /api/events (SSE),
//     so a synthetic event lands in the browser through the existing
//     pipe — no new transport, no new auth boundary.
//  2. The same event reaches any other in-process subscriber (audit
//     log, future controllers) that already watches the bus.
//
// Receiver is reactive : it doesn't keep state itself. Each agent
// re-publishes its full state on every tick (default 10 s), so a
// freshly-subscribed UI catches up inside one tick.
type StatusReceiver struct {
	subscribe NATSSubscribeFunc
	bus       eventBusPublisher
	logger    *log.Logger
}

// NATSSubscribeFunc opens a wildcard subscription on the firewall
// status subject pattern and pipes every message into the handler.
// Tests inject a stub that drives messages synchronously.
type NATSSubscribeFunc func(ctx context.Context, subjectPattern string, handler func(subject string, data []byte)) error

// eventBusPublisher is the narrow Publish surface needed to
// re-emit synthetic events. weft.EventBus satisfies it.
type eventBusPublisher interface {
	Publish(weft.PlatformEvent)
}

// NewStatusReceiver builds a StatusReceiver. nil logger →
// log.Default ; nil bus or subscribe is rejected at construction
// (would only surface at first message — fail fast instead).
func NewStatusReceiver(subscribe NATSSubscribeFunc, bus eventBusPublisher, logger *log.Logger) (*StatusReceiver, error) {
	if subscribe == nil {
		return nil, fmt.Errorf("firewallpub.NewStatusReceiver: nil subscribe func")
	}
	if bus == nil {
		return nil, fmt.Errorf("firewallpub.NewStatusReceiver: nil event bus")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &StatusReceiver{subscribe: subscribe, bus: bus, logger: logger}, nil
}

// Run subscribes to "weft.firewall.*.status" and blocks until ctx
// is cancelled. Decode failures log + drop ; the next tick from the
// same agent self-heals.
func (r *StatusReceiver) Run(ctx context.Context) error {
	return r.subscribe(ctx, "weft.firewall.*.status", func(subject string, data []byte) {
		r.handle(subject, data)
	})
}

// HandleMessage is the testable seam : feed it a raw subject + JSON
// payload and observe the bus.
func (r *StatusReceiver) HandleMessage(subject string, data []byte) {
	r.handle(subject, data)
}

func (r *StatusReceiver) handle(subject string, data []byte) {
	vmUUID := vmUUIDFromStatusSubject(subject)
	if vmUUID == "" {
		r.logger.Printf("firewallpub: status subject %q : unparseable vm uuid", subject)
		return
	}
	var status pod.FirewallStatus
	if err := json.Unmarshal(data, &status); err != nil {
		r.logger.Printf("firewallpub: status subject %q : decode: %v", subject, err)
		return
	}
	// Tick weft_firewall_status_events_total AFTER subject + JSON
	// parse pass : a counter labelled vm_uuid="" for a malformed
	// subject would explode cardinality and would not reflect a real
	// agent status. Empty Overall is legal (status payload from a
	// reconciler that never observed itself) ; we record it as-is.
	ensureRegistered()
	statusEventsTotal.WithLabelValues(vmUUID, status.Overall).Inc()
	r.bus.Publish(weft.PlatformEvent{
		Kind:    "firewall.status",
		Subject: vmUUID,
		Meta:    metaFromStatus(status),
	})
}

// vmUUIDFromStatusSubject extracts the VM UUID from a
// "weft.firewall.<uuid>.status" subject. Returns "" when the shape
// doesn't match (defensive : the wildcard subscription should never
// surface a non-matching subject).
func vmUUIDFromStatusSubject(subject string) string {
	const (
		prefix = "weft.firewall."
		suffix = ".status"
	)
	if len(subject) <= len(prefix)+len(suffix) {
		// Prefix + suffix would overlap. Negative slice math
		// would panic ; bail early.
		return ""
	}
	if !strings.HasPrefix(subject, prefix) || !strings.HasSuffix(subject, suffix) {
		return ""
	}
	mid := subject[len(prefix) : len(subject)-len(suffix)]
	if mid == "" || strings.Contains(mid, ".") {
		return ""
	}
	return mid
}

// metaFromStatus flattens a FirewallStatus into the string-string
// Meta map PlatformEvent carries. Numeric fields are strconv'd ;
// LastError is omitted when empty so the UI's "missing field"
// handling stays trivial.
func metaFromStatus(s pod.FirewallStatus) map[string]string {
	m := map[string]string{
		"Overall":         s.Overall,
		"RulesInstalled":  strconv.Itoa(s.RulesInstalled),
		"TableInstalled":  strconv.FormatBool(s.TableInstalled),
		"PublishedAtUnix": strconv.FormatInt(s.PublishedAtUnix, 10),
	}
	if s.LastError != "" {
		m["LastError"] = s.LastError
	}
	return m
}
