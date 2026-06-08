# Respawn monitors — HA topology

`weft v0.4.1` ships respawn V0.1.3 : every `weft agent` runs an
in-process **monitor** that watches the local VMs for unexpected exits,
respawns them through the SchedulingRule policy, and (when the etcd
backend is wired) participates in cross-host failover. This page is the
operator-facing summary of the topology — who watches what, what
guarantees you get, and how to verify it from the CLI.

## Topology in one paragraph

One monitor per host (= one per running `weft agent` process). All
monitors observe the same etcd prefix `/weft/coord/hosts/<host_uuid>`
where each agent registers a 10s-TTL lease refreshed every 3s. When a
host's lease expires, every surviving monitor sees the `HostDown`
event ; per `SchedulingRule`, etcd-concurrency elects exactly one
leader, and the leader claims the orphan VMs (flips `host_uuid` in
the registry + injects `SignalDown` into the local Reconciler →
StartVM). No N-way thrash, no SPOF on a single coordinator.

```
┌─── dc1 / AZ1 ────┐  ┌─── dc2 / AZ2 ────┐  ┌─── dc3 / AZ3 ────┐
│ weft agent       │  │ weft agent       │  │ weft agent       │
│  └─ Monitor M1   │  │  └─ Monitor M2   │  │  └─ Monitor M3   │
│     └─ poller    │  │     └─ poller    │  │     └─ poller    │
│     └─ subscr.   │  │     └─ subscr.   │  │     └─ subscr.   │
│     └─ election  │  │     └─ election  │  │     └─ election  │
└──────────────────┘  └──────────────────┘  └──────────────────┘
     lease M1              lease M2              lease M3
          ↓                    ↓                    ↓
   ┌────────────────── etcd quorum ──────────────────┐
   │  /weft/coord/hosts/{14226f04, 87b77efe, ...}    │
   │  /weft/coord/elect/respawn/<rule_uuid>          │
   │  vms (HCL blob, shared)                         │
   └─────────────────────────────────────────────────┘
```

## What the monitor catches

| Failure mode | Detector | Reaction |
|---|---|---|
| **VM exits on a healthy host** | Local poller reads `<vmDir>/exit.json` + `vm.pid` signal-0 + `/proc/<pid>/status` zombie state | `SignalDown` → Reconciler honours `RespawnPolicy.grace_period_ms` then `StartVM` locally |
| **`weft agent` wedges, host alive** | systemd `WatchdogSec=30s` with `sd_notify(WATCHDOG=1)` at 10s cadence | systemd kills + `Restart=always` (capped at `StartLimitBurst=10` per 2 min) |
| **Host dies entirely** | etcd lease expires after 10s ; surviving monitors get `HostDown` event from the HostWatcher | Leader election per `SchedulingRule` → leader calls `MigrateVM(uuid, localHostUUID)` then `SignalDown` → respawn locally |

The third row depends on quorum. With the default 10s TTL :
- **Detection latency** : ~7–13s (one keep-alive cycle + parser slack).
- **Claim + StartVM latency** : grace_period + backoff (configured per rule). On the lab cluster with `grace=3s`, `initial=1s`, the first respawn lands ~5–8s after detection.

## Verify : `weft monitor` CLI

The CLI talks straight to etcd ; no need to be inside an agent process.

```
$ weft monitor ls --etcd-endpoints=dc1:2379,dc2:2379,dc3:2379
HOST_UUID                             HOSTNAME   HYPERVISOR  STARTED_AT            UPTIME
14226f04-1187-4c86-8356-3d6e82b71366  dc2-r1-h1  qemu        2026-06-08T19:57:01Z  3m45s
87b77efe-3386-4233-852f-6af0bc1a211d  dc3-r1-h1  qemu        2026-06-08T19:57:02Z  3m44s
a777bdcf-14a3-464a-8164-6b4a3e2b222e  dc1-r1-h1  qemu        2026-06-08T19:57:00Z  3m46s

3 live monitor(s)
```

`weft monitor watch` streams `UP` / `DOWN` events in real time — useful
during a drill or while debugging a flapping host :

```
$ weft monitor watch --etcd-endpoints=dc1:2379,dc2:2379,dc3:2379
watching /weft/coord/hosts/ — Ctrl-C to exit
2026-06-08T19:41:23Z   DOWN  14226f04-1187-4c86-8356-3d6e82b71366  dc2-r1-h1
2026-06-08T19:41:55Z   UP    14226f04-1187-4c86-8356-3d6e82b71366  dc2-r1-h1
```

`weft monitor doctor` prints a verdict — green / yellow / red — based
on how many monitors are live vs the expected count :

```
$ weft monitor doctor --etcd-endpoints=dc1:2379,dc2:2379,dc3:2379
monitors live  : 3
monitors expected : 3
etcd members    : 3

HOSTNAME   HOST_UUID                             HYPERVISOR  UPTIME
dc1-r1-h1  a777bdcf-14a3-464a-8164-6b4a3e2b222e  qemu        12m
dc2-r1-h1  14226f04-1187-4c86-8356-3d6e82b71366  qemu        12m
dc3-r1-h1  87b77efe-3386-4233-852f-6af0bc1a211d  qemu        11m

verdict : OK — full HA capacity ; any single host can fail without losing monitor coverage.
```

`--expected` overrides the auto-inferred count (defaults to the etcd
member count, which usually matches the agent fleet size on small
clusters). For a 5-host fleet against a 3-member etcd cluster, pin
it : `--expected=5`.

Verdicts :

- **OK** — `live ≥ expected`. Full HA capacity ; any single host loss
  is absorbed.
- **DEGRADED** — `live` in `[ceil(expected/2)+1, expected)`. Failover
  still works but no head-room ; one more loss → CRITICAL.
- **CRITICAL** — `live` below the quorum floor. Surviving monitors
  can't elect a leader for cross-host claims. New host failures will
  go unclaimed until a monitor recovers.

## Prometheus

The agent exports `weft_monitors_live` (a single float gauge) on its
`/metrics` endpoint when `--metrics-listen` is configured and the
storage backend is etcd. Polled every 5s. Two alerting recipes :

```yaml
- alert: WeftMonitorsDegraded
  expr: weft_monitors_live < <expected_count>
  for: 30s
  annotations:
    summary: "Less than {{ <expected_count> }} weft monitors are live"

- alert: WeftMonitorsCritical
  expr: weft_monitors_live <= ceil(<expected_count> / 2)
  for: 10s
  annotations:
    summary: "Weft monitors below etcd quorum — cross-host failover paused"
```

(Hardcode `<expected_count>` to your fleet size ; Prometheus doesn't
have an arithmetic way to read the etcd member count.)

## Webui

The Infra portal's Overview page mounts a `MonitorsPanel` component
that calls `GET /api/monitors` every 5s. Each monitor's hostname,
hypervisor, version, and uptime are listed ; a daisyUI badge above
the list reports OK / DEGRADED / CRITICAL using the same logic as
`weft monitor doctor`.

`WEFT_WEBUI_EXPECTED_MONITORS` env var pins the expected count when
operators want it different from the etcd member count (the same
mismatch case as the CLI's `--expected` flag).

## How "monitor" relates to other concepts

- A **monitor** is the per-agent surveillance loop. There is exactly
  one per `weft agent` process. The number scales with the fleet — it
  is not a tier you provision separately.
- A **driver** is the OCI-pulled hypervisor plugin (`weft-driver-vz`,
  `weft-driver-qemu`, `weft-driver-vmd`, `weft-driver-dcs`) hosted
  inside the agent. Drivers are about *executing* VMs ; monitors are
  about *watching* them. One agent has 1+ drivers and exactly 1
  monitor.
- A **SchedulingRule** with `respawn{}` is the policy a monitor
  reconciles against. Monitors don't generate rules — they execute
  them.

## Failure modes for the monitor topology itself

| What broke | Symptom | Recovery |
|---|---|---|
| Single agent OOM-killed | `weft_monitors_live` drops by 1 ; systemd auto-restarts within `RestartSec=2s` ; lease re-grants in ~3s | Watch for repeat OOM (memory leak), bump `MemoryHigh=` if it's a healthy growth spike |
| Network partition isolates one DC | That DC's lease expires after 10s ; surviving monitors see DOWN ; the isolated agent can't reach etcd so its own writes pile up locally | Network heals → lease auto-renews ; queued writes apply linearizably (last-writer-wins is the registry rule) |
| etcd loses quorum (2 of 3 DCs down) | Surviving monitor can still serve reads from its local etcd peer ; elections block (no quorum) so no leader is elected | Bring back an etcd peer ; elections resume immediately ; pending claims fire |
| Monitor wedged in respawn loop (`StartVM` keeps failing) | Per-rule restart counter exhausts within `RespawnPolicy.window_ms` ; subsequent failures parked in `Cooldown` | Investigate root cause (driver crash, image pull failure, OOM on guest) ; `weft scheduling-rule update --no-respawn` to disable while debugging |

## Related

- [`docs/operations/ha-failover.md`](ha-failover.md) — fire-drill
  runbook against a full 3-DC cluster.
- [`docs/design/architecture.md`](../design/architecture.md) —
  topology + control-plane overview.
- Memory `project_respawn_v013_true_ha.md` — implementation notes.
