package main

// bus_drops_metric.go exposes the in-process event-bus drop counter
// as a Prometheus gauge. Drops happen when a subscriber's channel
// (buffer = 128) fills faster than the consumer drains it ; the
// LocalEventBus increments a monotonic counter per drop. Surface it
// so operators can alert on bus overload (a wedged consumer, a
// genuine event burst, or a misconfigured subscriber filter).

import (
	"context"
	"log/slog"
	"time"

	"github.com/openweft/weft"
	"github.com/prometheus/client_golang/prometheus"
)

// startBusDropsGauge registers a `weft_bus_dropped_total` gauge on
// the supplied Prometheus registry. The gauge is refreshed every 5s
// from the LocalEventBus's atomic counter. Pass nil bus or nil reg
// to disable (no-op).
//
// Returns a cancel that stops the refresh goroutine cleanly.
func startBusDropsGauge(reg *prometheus.Registry, bus weft.EventBus) func() {
	if reg == nil || bus == nil {
		return func() {}
	}
	local, ok := bus.(*weft.LocalEventBus)
	if !ok {
		// NATS bus has its own drop semantics ; not surfaced here.
		return func() {}
	}
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "weft_bus_dropped_total",
		Help: "Cumulative count of in-process PlatformEvents dropped because a subscriber's channel was full. A non-zero rate indicates a wedged consumer or genuine event burst overrunning the 128-deep buffer.",
	})
	if err := reg.Register(gauge); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			gauge = are.ExistingCollector.(prometheus.Gauge)
		} else {
			slog.Default().Warn("metrics: register weft_bus_dropped_total failed", "err", err)
			return func() {}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		gauge.Set(float64(local.DropTotal()))
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				gauge.Set(float64(local.DropTotal()))
			}
		}
	}()
	return cancel
}
