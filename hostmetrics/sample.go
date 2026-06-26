// Package hostmetrics samples per-host CPU / memory / network counters
// every 5s and publishes them as JSON to NATS subject
// `weft.host.<uuid>.metrics`. Consumers (weft-tui's host detail
// drawer, Grafana exporter, weft-doctor) subscribe and render time
// series without round-tripping to the agent for each refresh.
//
// Design ties :
//
//   - The NATS conn is provided by the caller — same pattern the
//     existing eventbus_nats / firewallpub / sharemount take, so we
//     reuse the agent's already-configured connection instead of
//     opening a second one.
//
//   - When the conn is nil (single-host dev, WEFT_NATS_URL unset) the
//     sampler degrades to a no-op : ticker still fires (cheap) but
//     Publish is skipped. Avoids special-casing at the call site.
//
//   - Per-platform /proc parsing lives in sample_linux.go ;
//     sample_other.go returns zeroed samples so the package builds
//     on macOS dev hosts without breaking the agent compile.
//
//   - One sampler per agent. The host_uuid + hostname are stamped on
//     every sample so subscribers fan-in cleanly without parsing
//     subject paths.
package hostmetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Sample is one observation broadcast on the NATS subject. Values are
// instantaneous at sampling time ; rates (NetRxBps / NetTxBps) are
// computed from the delta against the previous sample. Stable across
// versions — bump a new field rather than reuse a name.
type Sample struct {
	TsUnixNs      int64   `json:"ts_unix_ns"`
	HostUUID      string  `json:"host_uuid"`
	Hostname      string  `json:"hostname"`
	CPUPct        float64 `json:"cpu_pct"`         // 0-100 across all cores
	MemUsedBytes  uint64  `json:"mem_used_bytes"`  // total - available
	MemTotalBytes uint64  `json:"mem_total_bytes"` // /proc/meminfo MemTotal
	NetRxBps      uint64  `json:"net_rx_bps"`      // sum across non-loopback ifaces
	NetTxBps      uint64  `json:"net_tx_bps"`
}

// Publisher abstracts the NATS conn so tests can inject a recorder
// without dialing a server. nats.Conn satisfies this directly.
type Publisher interface {
	Publish(subject string, data []byte) error
}

// Sampler holds the bookkeeping for delta-based CPU + network
// rate computation. Run() loops until ctx cancels.
type Sampler struct {
	pub      Publisher
	subject  string
	hostUUID string
	hostname string
	interval time.Duration
	log      *slog.Logger

	// Last observed counters + timestamp ; used to compute deltas
	// on the next sample. Zero-valued on the first iteration so
	// the first published Sample reports cpu_pct=0 / net_*_bps=0
	// rather than a spike against an undefined baseline.
	lastCPU cpuCounters
	lastNet netCounters
	lastAt  time.Time
}

// Options is the constructor input. Subject overrides the default
// "weft.host.<uuid>.metrics" when callers want to share a NATS
// instance across multiple fleets. Interval defaults to 5s.
type Options struct {
	Subject  string
	Interval time.Duration
	Logger   *slog.Logger
}

// New builds a Sampler. pub may be nil — in that case the sampler
// still runs but skips Publish (useful in dev mode / tests).
func New(pub Publisher, hostUUID, hostname string, opts Options) *Sampler {
	s := &Sampler{
		pub:      pub,
		subject:  opts.Subject,
		hostUUID: hostUUID,
		hostname: hostname,
		interval: opts.Interval,
		log:      opts.Logger,
	}
	if s.subject == "" {
		s.subject = fmt.Sprintf("weft.host.%s.metrics", hostUUID)
	}
	if s.interval <= 0 {
		s.interval = 5 * time.Second
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Run blocks until ctx cancels, sampling + publishing on its
// configured interval. Read errors are logged and skipped — a
// transient /proc read failure shouldn't kill the loop.
func (s *Sampler) Run(ctx context.Context) {
	// Prime the deltas so the first emitted sample reports 0 instead
	// of a meaningless rate against time-zero counters.
	if cpu, err := readCPU(); err == nil {
		s.lastCPU = cpu
	}
	if net, err := readNet(); err == nil {
		s.lastNet = net
	}
	s.lastAt = time.Now()

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			sample, err := s.collect(now)
			if err != nil {
				s.log.Warn("hostmetrics: sample failed", "err", err)
				continue
			}
			if s.pub == nil {
				continue
			}
			blob, err := json.Marshal(sample)
			if err != nil {
				s.log.Warn("hostmetrics: marshal", "err", err)
				continue
			}
			if err := s.pub.Publish(s.subject, blob); err != nil {
				s.log.Warn("hostmetrics: publish", "subject", s.subject, "err", err)
			}
		}
	}
}

// collect builds a single Sample from the current /proc values and
// the cached deltas. Exported via tests as collectAt(t) so the
// rate math is verifiable without sleeping.
func (s *Sampler) collect(now time.Time) (Sample, error) {
	cpu, err := readCPU()
	if err != nil {
		return Sample{}, fmt.Errorf("cpu: %w", err)
	}
	mem, err := readMem()
	if err != nil {
		return Sample{}, fmt.Errorf("mem: %w", err)
	}
	net, err := readNet()
	if err != nil {
		return Sample{}, fmt.Errorf("net: %w", err)
	}
	elapsed := now.Sub(s.lastAt).Seconds()
	if elapsed < 0.1 {
		elapsed = 0.1
	}
	cpuPct := cpuDeltaPct(s.lastCPU, cpu)
	netRxBps := uint64(deltaPerSec(s.lastNet.rxBytes, net.rxBytes, elapsed))
	netTxBps := uint64(deltaPerSec(s.lastNet.txBytes, net.txBytes, elapsed))
	s.lastCPU = cpu
	s.lastNet = net
	s.lastAt = now
	return Sample{
		TsUnixNs:      now.UnixNano(),
		HostUUID:      s.hostUUID,
		Hostname:      s.hostname,
		CPUPct:        cpuPct,
		MemUsedBytes:  mem.used,
		MemTotalBytes: mem.total,
		NetRxBps:      netRxBps,
		NetTxBps:      netTxBps,
	}, nil
}

// cpuCounters mirrors the first line of /proc/stat (sums across all
// cores). The /proc layout is fixed since kernel 2.6 ; the trailing
// fields (irq, softirq, steal, guest, guest_nice) we only consume
// through field 7 — newer additions are ignored on purpose so a
// future kernel can't break the parser.
type cpuCounters struct {
	user, nice, sys, idle, iowait, irq, softirq, steal uint64
}

// netCounters aggregates rx_bytes + tx_bytes across all non-loopback
// interfaces. Per-interface breakdown isn't published in V1 — the
// hosts detail drawer's primary need is "is this host saturating its
// link?", a question the aggregate answers cleanly.
type netCounters struct {
	rxBytes, txBytes uint64
}

type memCounters struct {
	total, used uint64
}

// cpuDeltaPct converts two cpuCounters readings into a 0-100 % value :
// (totalDelta - idleDelta) / totalDelta. A first iteration where the
// "old" counters are zero ends up reporting close to 100% (because
// idleDelta = new.idle - 0 = new.idle, far less than totalDelta) —
// the Run() prime step above avoids that.
func cpuDeltaPct(old, new cpuCounters) float64 {
	oldTotal := old.user + old.nice + old.sys + old.idle + old.iowait + old.irq + old.softirq + old.steal
	newTotal := new.user + new.nice + new.sys + new.idle + new.iowait + new.irq + new.softirq + new.steal
	if newTotal <= oldTotal {
		return 0
	}
	totalDelta := newTotal - oldTotal
	idleDelta := new.idle - old.idle
	busy := totalDelta - idleDelta
	if busy > totalDelta {
		// Counters wrapped or got rebooted ; clamp.
		return 0
	}
	return float64(busy) / float64(totalDelta) * 100
}

// deltaPerSec normalises a counter delta to a per-second rate. Guards
// against negative deltas (counter resets on reboot) and against
// sub-100ms elapsed (already clamped upstream but defensive).
func deltaPerSec(oldV, newV uint64, elapsed float64) float64 {
	if newV < oldV {
		return 0
	}
	if elapsed <= 0 {
		return 0
	}
	return float64(newV-oldV) / elapsed
}
