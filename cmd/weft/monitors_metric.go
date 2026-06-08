package main

// monitors_metric.go exposes the live-monitor count as a Prometheus
// gauge sourced from the etcd-coord liveness leases. The number of
// monitors == number of weft agents currently holding a non-expired
// lease at /weft/coord/hosts/<host_uuid>. One per host, by design.
//
// Operators alert on a sudden drop (a DC partition or rack outage)
// or on the gauge staying below a configured floor for too long.

import (
	"context"
	"log/slog"
	"time"

	"github.com/openweft/weft/etcdcoord"
	"github.com/prometheus/client_golang/prometheus"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// startMonitorsGauge registers a `weft_monitors_live` gauge on the
// supplied Prometheus registry and refreshes it every 5s by counting
// keys under /weft/coord/hosts/. No-op when etcdCli is nil (file-
// storage backend, in which case "monitors" doesn't apply) or when
// the registry is nil (operator opted out of /metrics).
//
// Returns a cancel that stops the refresh goroutine cleanly. Always
// non-nil so the caller can defer it unconditionally.
func startMonitorsGauge(reg *prometheus.Registry, etcdCli *clientv3.Client) func() {
	if reg == nil || etcdCli == nil {
		return func() {}
	}
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "weft_monitors_live",
		Help: "Number of weft-agent monitors currently holding an etcd-coord liveness lease. One per agent ; equals the number of hosts reachable in the cluster.",
	})
	// Best-effort register : a duplicate registration shouldn't crash
	// the agent (e.g. a transient reload path).
	if err := reg.Register(gauge); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			gauge = are.ExistingCollector.(prometheus.Gauge)
		} else {
			slog.Default().Warn("metrics: register weft_monitors_live failed", "err", err)
			return func() {}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		// Snapshot immediately so the gauge isn't 0 at the first scrape.
		gauge.Set(float64(countMonitors(ctx, etcdCli)))
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				gauge.Set(float64(countMonitors(ctx, etcdCli)))
			}
		}
	}()
	return cancel
}

func countMonitors(ctx context.Context, cli *clientv3.Client) int {
	gctx, gcancel := context.WithTimeout(ctx, 2*time.Second)
	defer gcancel()
	resp, err := cli.Get(gctx, etcdcoord.HostsPrefix, clientv3.WithPrefix(), clientv3.WithCountOnly(), clientv3.WithSerializable())
	if err != nil {
		return -1
	}
	return int(resp.Count)
}
