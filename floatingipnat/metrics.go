// metrics.go owns the Prometheus surface this package exposes for
// host-side NAT reconcile visibility :
//
//   - weft_fip_nat_apply_total{result}        — Reconciler.Apply call counter
//   - weft_fip_nat_rules_installed            — last-successful-Apply rule count
//   - weft_fip_nat_apply_duration_seconds     — Apply latency histogram
//
// Operators alert on a stalled reconcile loop with PromQL like :
//
//	rate(weft_fip_nat_apply_total[5m]) == 0
//
// and on convergence drift with :
//
//	weft_fip_nat_rules_installed != on() platform_floating_ip_total
//
// (the matching `platform_floating_ip_total` lives in the catalogue
// reconciler — out of scope here, but the relationship is the alerting
// shape we instrumented for).
//
// Registration policy mirrors firewallpub/metrics.go : a package-
// level Register(prometheus.Registerer) lets cmd-side wiring scope the
// collectors, otherwise the first Apply lazily binds them to
// prometheus.DefaultRegisterer. Process-wide singletons : every
// LinuxReconciler / StubReconciler on the host shares the same three
// collectors (we only run one Reconciler per host, so the cardinality
// is bounded).

package floatingipnat

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// applyTotal counts Apply() invocations, labelled by result :
	//   ok  — Apply returned nil
	//   err — Apply returned a non-nil error (ValidateMappings reject,
	//         nftables flush failure, malformed netip parse, ...)
	// The counter is incremented exactly once per Apply call, AFTER
	// the underlying netlink (or stub) work returns, so the result
	// label always reflects the actual outcome.
	applyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "weft_fip_nat_apply_total",
		Help: "Total Reconciler.Apply invocations on this host, labelled by result (ok|err). The result reflects whether the nftables flush (linux) or stub recording (other) succeeded.",
	}, []string{"result"})

	// rulesInstalled is a gauge of the rule count derived from the
	// LAST successful Apply. A failed Apply does NOT touch the gauge —
	// the previous value is the closest description of the in-kernel
	// state. The gauge is set from the input mapping count (one
	// mapping = one DNAT + one SNAT rule, but the operator-facing
	// quantity is "mappings" — keep the label-free convention).
	rulesInstalled = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "weft_fip_nat_rules_installed",
		Help: "Number of floating-IP NAT mappings installed by the last successful Reconciler.Apply on this host. A failed Apply leaves the value untouched.",
	})

	// applyDuration is a histogram of Apply() latency, bucketed
	// against the default prometheus buckets (5 ms → 10 s). Linux
	// path = netlink batch dominated, typically sub-50ms ; stub path
	// (darwin / test) sub-millisecond. The same histogram covers both
	// so a drift in either backend shows up.
	applyDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "weft_fip_nat_apply_duration_seconds",
		Help:    "Reconciler.Apply latency histogram on this host. Default Prometheus buckets cover the netlink (linux) + stub (other) bands.",
		Buckets: prometheus.DefBuckets,
	})

	// regOnce gates the one-shot Register call. Same semantics as
	// firewallpub's regOnce — first caller wins, subsequent calls are
	// no-ops. Tests rely on Write-style reads, not gather, so this is
	// safe to share across the test binary.
	regOnce sync.Once
)

// Register binds the floatingipnat collectors to reg. Idempotent :
// only the first call has effect. Passing nil falls back to
// prometheus.DefaultRegisterer so back-compat callers (tests, bare
// daemon main()s) still produce values. Mirrors firewallpub.Register.
func Register(reg prometheus.Registerer) error {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	var err error
	regOnce.Do(func() {
		if e := reg.Register(applyTotal); e != nil {
			err = e
			return
		}
		if e := reg.Register(rulesInstalled); e != nil {
			err = e
			return
		}
		if e := reg.Register(applyDuration); e != nil {
			err = e
			return
		}
	})
	return err
}

// ensureRegistered is the lazy fallback the instrumentation hot path
// calls before observing. If a cmd-side caller already invoked
// Register, the sync.Once is consumed and this is a no-op.
func ensureRegistered() {
	_ = Register(prometheus.DefaultRegisterer)
}

// recordApply is the single instrumentation seam the Reconciler
// implementations call AFTER their Apply body returns. Centralising
// the three observations here keeps reconciler_linux.go and
// reconciler_other.go in sync — a future Reconciler impl only has to
// call this with (mappings, err, elapsed) to land on the right metrics.
//
// rulesInstalled is set from len(mappings) ONLY when err == nil ; a
// failed Apply leaves the gauge at the previous successful value
// (matches the contract documented above).
func recordApply(mappings []NATMapping, err error, durSeconds float64) {
	ensureRegistered()
	result := "ok"
	if err != nil {
		result = "err"
	}
	applyTotal.WithLabelValues(result).Inc()
	applyDuration.Observe(durSeconds)
	if err == nil {
		rulesInstalled.Set(float64(len(mappings)))
	}
}
