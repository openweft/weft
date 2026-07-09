package main

// zombiegc_wire.go bridges the in-repo zombiegc Reconciler into the
// `weft agent` boot path. Spawns a goroutine that sweeps the VM
// registry every WEFT_ZOMBIE_GC_SWEEP_INTERVAL (default 5min), and
// publishes a Prometheus gauge `weft_vm_zombies` labelled by kind so
// operators can alert on accumulation.
//
// Tuning : WEFT_ZOMBIE_GC_CI_GRACE (default 1h) — how long a CI
// zombie stays marked before auto-delete. Match this to your CI
// job churn pattern : 1h is conservative for batch jobs running
// minutes ; shorten to 10m for high-velocity GitLab runners.

import (
	"context"
	"log/slog"
	"os"
	"time"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft/zombiegc"
	"github.com/prometheus/client_golang/prometheus"
)

// startZombieGC wires the GC + its Prometheus gauge. Returns the
// running Reconciler (for the gRPC GetZombieReport handler to query)
// and a cancel fn that stops the sweep goroutine cleanly on agent
// shutdown.
func startZombieGC(reg *prometheus.Registry, a weft.VZAdapter) (*zombiegc.Reconciler, func()) {
	if a == nil {
		return nil, func() {}
	}
	opts := zombiegc.Options{
		CIGracePeriod: envDuration("WEFT_ZOMBIE_GC_CI_GRACE", 1*time.Hour),
		SweepInterval: envDuration("WEFT_ZOMBIE_GC_SWEEP_INTERVAL", 5*time.Minute),
		HostDownGrace:  envDuration("WEFT_ZOMBIE_GC_HOST_DOWN_GRACE", 60*time.Second),
		OrphanDirGrace: envDuration("WEFT_ZOMBIE_GC_ORPHAN_DIR_GRACE", 5*time.Minute),
		// 2026-06-23 : default to 1h auto-delete for phantom vmDirs
		// (registry record missing, disk artifact left behind by a
		// failed deploy / manual rm / etc.). The user's directive
		// after seeing "infra-etcd-dc{7,42,55}" + a stale loom-server
		// linger across restarts : "si on est capable de dire que ce
		// sont des fantomes on doit etre capable de cicatriser tout
		// seul, quitte a avoir un weft-gc pour ca". 1h is short
		// enough that phantoms disappear within an operator's shift,
		// long enough to dodge a slow RegisterMicroVM in flight.
		// Operators who want the old mark-only behaviour set
		// WEFT_ZOMBIE_GC_ORPHAN_DIR_DELETE_AFTER=0.
		OrphanDirAutoDeleteAfter: envDuration("WEFT_ZOMBIE_GC_ORPHAN_DIR_DELETE_AFTER", 1*time.Hour),
		Logger:        slog.Default(),
	}
	// Liveness probe : reuse the same VMStatusReader logic the
	// respawn subscriber uses, so both subsystems see the same
	// truth (exit.json takes precedence over pid liveness).
	probe := respawnStatus{adp: a}
	r := zombiegc.New(a, probe, localHostUUID(a), opts)

	// Prometheus gauge labelled by zombie kind, optional — runs only
	// when metrics are enabled on this agent. Nil reg = sweep still
	// happens, just no /metrics surface.
	var gauge *prometheus.GaugeVec
	var deletedCounter prometheus.Counter
	if reg != nil {
		gauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "weft_vm_zombies",
			Help: "VMs currently flagged as zombies by the agent's zombiegc reconciler, labelled by kind (local|ci_cross_host|ha_cross_host|orphan_project). A non-zero, growing local count points to driver crashes ; growing ha_cross_host suggests a respawn rule misconfigured.",
		}, []string{"kind"})
		if err := reg.Register(gauge); err != nil {
			if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
				gauge = are.ExistingCollector.(*prometheus.GaugeVec)
			} else {
				slog.Default().Warn("metrics: register weft_vm_zombies failed", "err", err)
			}
		}
		deletedCounter = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "weft_vm_zombies_deleted_total",
			Help: "Cumulative count of CI zombies the zombiegc reconciler auto-deleted after the configured grace period.",
		})
		if err := reg.Register(deletedCounter); err != nil {
			if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
				deletedCounter = are.ExistingCollector.(prometheus.Counter)
			} else {
				slog.Default().Warn("metrics: register weft_vm_zombies_deleted_total failed", "err", err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	// Separate ticker to refresh the gauges from Stats — decoupled
	// from the sweep cadence so the metric is fresh even between
	// sweeps (operator dashboards poll every 15-30s typically).
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		var lastDeleted uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if gauge == nil {
					continue
				}
				s := r.StatsSnapshot()
				for _, k := range []zombiegc.ZombieKind{
					zombiegc.ZombieLocal,
					zombiegc.ZombieCICrossHost,
					zombiegc.ZombieHACrossHost,
					zombiegc.ZombieOrphanProject,
					zombiegc.ZombieOrphanDir,
				} {
					gauge.WithLabelValues(string(k)).Set(float64(s.ZombiesByKind[k]))
				}
				if s.DeletedTotal > lastDeleted {
					deletedCounter.Add(float64(s.DeletedTotal - lastDeleted))
					lastDeleted = s.DeletedTotal
				}
			}
		}
	}()
	slog.Default().Info("zombiegc : started",
		"sweep_interval", opts.SweepInterval,
		"ci_grace", opts.CIGracePeriod,
		"host_down_grace", opts.HostDownGrace,
		"orphan_dir_grace", opts.OrphanDirGrace,
		"orphan_dir_delete_after", opts.OrphanDirAutoDeleteAfter,
	)
	return r, cancel
}

// envDuration parses a duration from the env var if set, else
// returns the default. Bad values log a warning + fall back.
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Default().Warn("zombiegc : invalid env duration, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	return d
}
