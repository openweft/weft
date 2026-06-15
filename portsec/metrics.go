// metrics.go owns the Prometheus surface portsec exposes for
// host-side anti-spoof visibility :
//
//   - weft_portsec_apply_total{result}       — Reconciler.Apply counter
//   - weft_portsec_rules_installed           — last-successful-Apply rule count
//   - weft_portsec_apply_duration_seconds    — Apply latency histogram
//
// Mirrors floatingipnat/metrics.go exactly so the weft-network-plane
// Grafana dashboard can query the same families across both
// reconcilers. One Reconciler per host → bounded cardinality, no
// per-tap labels in this layer (the rule count gauge collapses
// per-tap detail to one number, which is the operator-visible
// quantity).

package portsec

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	applyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "weft_portsec_apply_total",
		Help: "Total Reconciler.Apply invocations on this host, labelled by result (ok|err).",
	}, []string{"result"})

	rulesInstalled = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "weft_portsec_rules_installed",
		Help: "Number of anti-spoof rules installed by the last successful Reconciler.Apply on this host. A failed Apply leaves the value untouched.",
	})

	applyDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "weft_portsec_apply_duration_seconds",
		Help:    "Reconciler.Apply latency histogram on this host. Default Prometheus buckets cover the netlink (linux) + stub (other) bands.",
		Buckets: prometheus.DefBuckets,
	})

	regOnce sync.Once
)

// Register binds the portsec collectors to reg. Idempotent ; nil reg
// falls back to prometheus.DefaultRegisterer.
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

func ensureRegistered() { _ = Register(prometheus.DefaultRegisterer) }

// recordApply is the single instrumentation seam both reconciler
// implementations call AFTER their Apply body returns.
func recordApply(rules []AntispoofRule, err error, durSeconds float64) {
	ensureRegistered()
	result := "ok"
	if err != nil {
		result = "err"
	}
	applyTotal.WithLabelValues(result).Inc()
	applyDuration.Observe(durSeconds)
	if err == nil {
		rulesInstalled.Set(float64(len(rules)))
	}
}
