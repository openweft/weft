// metrics.go owns the Prometheus surface dhcpd exposes for
// host-side DHCPv4 server visibility :
//
//   - weft_dhcpd_packets_total{outcome}   — every inbound packet,
//                                            labelled by what we did
//   - weft_dhcpd_handle_duration_seconds  — parse + decide + send time
//
// Outcomes :
//
//   - offer / ack / nak       — we sent a reply of that type
//   - drop_parse_err          — malformed wire packet
//   - drop_unknown_mac        — Source.Resolve returned false
//   - drop_decide_err         — Lease validation or BuildReply failed
//   - drop_unsupported        — message type we don't handle
//                              (DECLINE/RELEASE/INFORM/server reply)
//   - send_err                — wire-side write to UDP/68 failed
//
// One Server per host bridge → bounded cardinality, no per-mac labels
// (clients are tracked at the lease layer ; the metrics here are
// load + health, not directory).

package dhcpd

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	packetsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "weft_dhcpd_packets_total",
		Help: "Total DHCPv4 packets processed on this host's bridges, labelled by outcome (offer|ack|nak|drop_parse_err|drop_unknown_mac|drop_decide_err|drop_unsupported|send_err).",
	}, []string{"outcome"})

	handleDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "weft_dhcpd_handle_duration_seconds",
		Help:    "Parse + Decide + send latency per inbound packet. Default Prometheus buckets cover sub-ms (Decide stub-mode) → 10s outliers.",
		Buckets: prometheus.DefBuckets,
	})

	regOnce sync.Once
)

// Register binds the dhcpd collectors to reg. Idempotent ; nil reg
// falls back to prometheus.DefaultRegisterer.
func Register(reg prometheus.Registerer) error {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	var err error
	regOnce.Do(func() {
		if e := reg.Register(packetsTotal); e != nil {
			err = e
			return
		}
		if e := reg.Register(handleDuration); e != nil {
			err = e
			return
		}
	})
	return err
}

func ensureRegistered() { _ = Register(prometheus.DefaultRegisterer) }

// recordPacket bumps the packets-total counter for outcome.
func recordPacket(outcome string) {
	ensureRegistered()
	packetsTotal.WithLabelValues(outcome).Inc()
}

// recordHandleDuration observes the per-packet latency.
func recordHandleDuration(durSeconds float64) {
	ensureRegistered()
	handleDuration.Observe(durSeconds)
}
