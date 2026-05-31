# Performance / load-test harness

This document covers `scripts/perf/`, a thin shell harness that exercises
the three load paths an operator cares about before rolling a release or
before resizing a cluster:

1. **microVM bring-up** — how fast does `register → start → running` run
   when you do it in parallel?
2. **Mesh fanout** — once a fresh VM is running, how long until it can
   actually talk to every existing peer on the overlay?
3. **etcd write rate** — what sustained req/s and P99 latency does the
   weft state store actually hit on this hardware?

The harness produces numbers. **Reading them against a baseline is
operator judgement, not a CI gate.** Perf measurements are inherently
noisy (host load, kernel image cache, network jitter, the moon's phase) —
don't wire them into a green/red check ; wire them into a runbook.

## Wiring

All three scripts are exposed as `task` targets so the invocation
matches the rest of the daemon's developer ergonomics. They expect
the `weft` CLI on `$PATH` and a reachable cluster (single-host or 3-DC ;
both work).

```
task perf:bringup N=10
task perf:bringup N=100
task perf:bringup N=1000

task perf:mesh N=100

task perf:etcd-write
```

Override the underlying knobs by calling the scripts directly:

```
./scripts/perf/bring-up-N.sh 100 --parallel 32 --image my-fixture:tag
./scripts/perf/mesh-fanout.sh 100 --peer-port 22 --timeout 90s
./scripts/perf/etcd-write-rate.sh --total 50000 --parallel 32
```

## What each script measures

### `bring-up-N.sh` — VM cold start under fan-out load

Issues `weft microvm register` for N VMs in parallel (xargs `-P`),
follows each with `start` and `wait --state=running`, and writes a
per-VM CSV of register / start / running / total milliseconds.

When to run:

- **Capacity planning** — pick the largest batch size you expect a
  scheduler burst to land on a single host, and verify the wall-clock
  is within your SLO.
- **Pre-release sanity** — diff numbers against the last tagged release
  on the same hardware. Regressions of >20% on the `register` or
  `running` phase deserve a bisect.
- **Driver-plugin regressions** — `start` time is dominated by the
  driver-plugin RPC (VZ or QEMU). A spike there points at the driver,
  not at the agent or etcd.

What "good" looks like, on a single Apple-silicon host with a warm
kernel cache:

| Phase    | Target |
|---       |---|
| register | < 100 ms |
| start    | < 500 ms |
| running  | < 5 s (cold start, includes guest agent dial-back) |
| total    | < 5.5 s |

For N=100 in parallel on the same host, wall-clock should be < 30 s
(register pipelined, start serialized by the driver, running mostly
bounded by guest boot). Above that, you are virtio-fs / 9p saturated.

### `mesh-fanout.sh` — time-to-mesh

Registers one extra "probe" VM, then for each peer runs
`weft microvm exec <probe> -- nc -z <peer-ip> 22` in a tight loop
until success or timeout. Reports P50 / P95 / P99 milliseconds for
peer-reach time. Probe VM is deleted on exit.

When to run:

- **After a fleet resize** — confirm new VMs picked up every existing
  peer before declaring the resize done.
- **WireGuard regression suspicion** — if a release touched
  `vendor/github.com/grpc-transports/wireguard`, run before / after
  with N=100 and diff.
- **Cross-DC mesh validation** — run on a 3-DC bring-up to confirm
  pubkey gossip is converging across DCs and not just within one.

What "good" looks like:

| N    | P50    | P95    | P99    |
|---   |---     |---     |---     |
| 10   | < 2 s  | < 5 s  | < 8 s  |
| 100  | < 5 s  | < 30 s | < 60 s |
| 1000 | < 15 s | < 90 s | (treat as soft cap, gossip dominates) |

### `etcd-write-rate.sh` — state-store throughput

Drives a configurable number of `etcdctl txn` writes (compare-revision
+ put — same code path the weft state uses, not a raw put) and reports
rate plus latency percentiles. Cleans up the test prefix on exit.

When to run:

- **Pre-reshuffle** — before a `weft up`-driven topology change, verify
  the quorum can absorb the planner's write burst.
- **Storage health** — sudden P99 jumps from <50ms to >100ms point at
  a degraded disk, not at weft.
- **3-DC quorum sanity** — run from each DC's local etcd endpoint and
  diff. Asymmetric numbers point at a flaky DC link.

What "good" looks like (3-node quorum on local SSD):

| Metric | Target |
|---     |---     |
| Rate (parallel=16) | > 2000 req/s |
| P50 latency | < 8 ms |
| P99 latency | < 50 ms |

## Common bottlenecks and mitigations

- **Kernel image pull rate**. First-time bring-up on a host with no
  cached kernel will serialize on the OCI artifact pull
  (`weft microvm pull-kernel`). Pre-warm with
  `task build` followed by an explicit pull-kernel ; or seed the cache
  out-of-band. The `running` phase will look pathological until you do.
- **virtio-fs / 9p saturation**. On macOS hosts, QEMU microVMs use 9p
  for the rootfs share (virtiofsd is Linux-only) and 9p is roughly
  half the throughput of virtio-fs. Bring-up of 100+ VMs sharing a
  host will queue on the 9p server ; spread across hosts.
- **etcd quorum write amplification**. Every `register` triggers an
  etcd write ; every state transition triggers another. At N=1000
  you're doing >3000 writes in a tight window — if `etcd-write-rate.sh`
  reports <1000 req/s on this cluster, you will get bring-up timeouts
  long before the driver is the bottleneck.
- **OIDC token refresh**. The `weft microvm register` calls re-validate
  the caller's token. If the OIDC issuer is on a slow link, this
  serializes the bring-up. Cache locally if you can.
- **xargs `-P` host limits**. Past `-P 64` you hit per-process ulimits
  on the host running the harness, not on the cluster. Raise the harness
  host's ulimit, or scale out the harness to multiple hosts.

## Reading the numbers

Two rules:

1. **Always compare against a baseline on the same hardware.** A "good"
   number on a beefy 3-DC bare-metal cluster is a catastrophe on a
   single laptop, and vice versa. Pin a baseline CSV in your runbook
   and diff against it.
2. **Three runs, take the median.** A single run can be off by 2x for
   reasons unrelated to weft (background OS work, NTP sync, etc.).

If the numbers look bad and you don't know why: run all three scripts
in sequence. The one with the worst regression points at the layer
that broke.
