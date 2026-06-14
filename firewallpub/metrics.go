// metrics.go owns the small Prometheus surface this package exposes
// for operator visibility :
//
//   - weft_firewall_publishes_total{kind, vm_uuid}     — Publisher.publishOne
//   - weft_firewall_status_events_total{vm_uuid, overall} — StatusReceiver.handle
//
// Operators alert on starved / silent publishers with PromQL like :
//
//	rate(weft_firewall_publishes_total[5m]) == 0
//
// and on Degraded fleets with :
//
//	sum by (overall) (rate(weft_firewall_status_events_total[5m]))
//
// Registration policy mirrors cmd/weft's rpc_metrics.go : a package-
// level Register(prometheus.Registerer) lets the cmd-side wiring scope
// the collectors to its own Registry (same Registry-not-Default policy
// the rest of the daemon follows). If the cmd never calls Register,
// the first increment lazily binds the collectors to
// prometheus.DefaultRegisterer so existing call sites that don't wire
// metrics still produce numbers on /debug/metrics endpoints.
//
// Process-wide singletons : the package shares one set of collectors
// across every Publisher / StatusReceiver in the binary, mirroring how
// rpc_metrics.go keeps `weft_rpc_total` global. Tests read counter
// values via the Write(*dto.Metric) seam — see metrics_test.go.

package firewallpub

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// publishesTotal counts every per-VM publish from publishOne,
	// labelled by the source event kind ("security_group.rules_updated",
	// "port.created", ...) and the target VM UUID. A flat zero on a
	// known-busy VM signals the event pipeline upstream of the publisher
	// has stalled.
	publishesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "weft_firewall_publishes_total",
		Help: "Total per-VM firewall publishes emitted by firewallpub.Publisher, labelled by the source PlatformEvent kind (security_group.rules_updated, port.created, ...) and the target VM UUID.",
	}, []string{"kind", "vm_uuid"})

	// statusEventsTotal counts every decoded status message the
	// host-side StatusReceiver re-emits on the event bus, labelled by
	// VM UUID and Overall state ("Healthy" / "Degraded"). A spike in
	// Degraded is the operator's first signal that a guest reconciler
	// is unhappy.
	statusEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "weft_firewall_status_events_total",
		Help: "Total FirewallStatus messages received from in-guest firewallstatus emitters, labelled by VM UUID and Overall state (Healthy / Degraded / empty when the agent omitted it).",
	}, []string{"vm_uuid", "overall"})

	// regOnce gates the one-shot Register call : either the cmd-side
	// wiring binds the collectors to a scoped Registry, or the first
	// increment falls back to DefaultRegisterer. Subsequent Register
	// calls are no-ops (matches sync.Once semantics).
	regOnce sync.Once
)

// Register binds the firewallpub collectors to reg. Idempotent : only
// the first call has effect, so cmd-side wiring should call it before
// the first event reaches Publisher.publishOne / StatusReceiver.handle.
// Passing nil falls back to prometheus.DefaultRegisterer so callers can
// disable scoping with a literal `Register(nil)`.
//
// Returns the first registration error if any ; subsequent calls
// return nil. Callers that need to detect already-registered errors
// across multiple test binaries should reset a process by spinning a
// fresh subprocess — this package intentionally does not expose a
// reset hook.
func Register(reg prometheus.Registerer) error {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	var err error
	regOnce.Do(func() {
		if e := reg.Register(publishesTotal); e != nil {
			err = e
			return
		}
		if e := reg.Register(statusEventsTotal); e != nil {
			err = e
			return
		}
	})
	return err
}

// ensureRegistered is the lazy fallback the instrumentation hot path
// calls before incrementing. If a cmd-side caller already invoked
// Register, the sync.Once is consumed and this is a no-op ; otherwise
// the collectors bind to DefaultRegisterer so back-compat callers
// (tests, mini main()s) still produce values.
func ensureRegistered() {
	_ = Register(prometheus.DefaultRegisterer)
}
