// metrics.go owns the Prometheus surface portqos exposes for
// host-side bandwidth-cap visibility :
//
//   - weft_portqos_apply_total{result}       — Reconciler.Apply counter
//   - weft_portqos_specs_installed           — last-successful-Apply spec count
//   - weft_portqos_apply_duration_seconds    — Apply latency histogram
//
// Mirrors floatingipnat / portsec — the weft-network-plane Grafana
// dashboard queries the same family across all three reconcilers.

package portqos

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	applyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "weft_portqos_apply_total",
		Help: "Total Reconciler.Apply invocations on this host, labelled by result (ok|err).",
	}, []string{"result"})

	specsInstalled = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "weft_portqos_specs_installed",
		Help: "Number of QoS specs installed by the last successful Reconciler.Apply on this host. A failed Apply leaves the value untouched.",
	})

	applyDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "weft_portqos_apply_duration_seconds",
		Help:    "Reconciler.Apply latency histogram on this host. Default Prometheus buckets cover the netlink (linux) + stub (other) bands.",
		Buckets: prometheus.DefBuckets,
	})

	regOnce sync.Once
)

// Register binds the portqos collectors to reg. Idempotent ; nil reg
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
		if e := reg.Register(specsInstalled); e != nil {
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

func recordApply(specs []PortQoS, err error, durSeconds float64) {
	ensureRegistered()
	result := "ok"
	if err != nil {
		result = "err"
	}
	applyTotal.WithLabelValues(result).Inc()
	applyDuration.Observe(durSeconds)
	if err == nil {
		specsInstalled.Set(float64(len(specs)))
	}
}
