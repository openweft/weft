# Rolling upgrade runbook (weft-agent, 3-DC cluster)

> **TL;DR** — weft-agent on each host is a systemd unit (`weft-agent.service`
> per `examples/cloud-init/debian-host.yaml`). Etcd quorum lets you bounce
> one host at a time. Snapshot etcd, canary host #1, watch metrics for 10
> minutes, then progress to #2, then #3. Roll-back = pin the previous tag
> and `systemctl restart`. Cordon-on-drain is **not yet implemented** in
> v0.1.0 — see [Cordon implementation note](#cordon-implementation-note).

This runbook covers moving a live 3-DC cluster from `ghcr.io/openweft/weft:v0.1.0`
to a later GHCR tag (`v0.1.1`, `v0.2.0`, …) with **zero control-plane
downtime** for user VMs. Read it once end-to-end before doing your first
production roll.

## 1. Upgrade model

- weft-agent is the **host-side daemon**. Every host in the cluster runs
  exactly one `weft-agent.service` instance (see the systemd unit shipped
  by `examples/cloud-init/debian-host.yaml`).
- An upgrade is a **controlled rolling restart**: pull the new image,
  bounce the unit, observe, move on.
- The **control plane stays up during the per-host restart** because
  etcd holds quorum (2/3) while one agent is briefly absent. Running
  user microVMs are unaffected — they're owned by the host kernel +
  driver-plugin process tree, not by the agent's own lifetime.
- Bounce **one host at a time**. Never restart two simultaneously: that
  drops you to 1/3 etcd nodes and the cluster goes read-only.

## 2. Pre-flight checklist

Run *all* of these before touching any host. If any fails, stop and
remediate first.

- [ ] **etcd healthy on all 3 endpoints**:
  ```
  etcdctl --endpoints=$E1,$E2,$E3 endpoint status --write-out=table
  etcdctl --endpoints=$E1,$E2,$E3 endpoint health
  ```
  All three rows green, one leader, no alarms.
- [ ] **`/metrics` reachable** on each host (`curl -sf
  https://<host>:9402/metrics | head -1`). Used by the dashboards you'll
  watch during the roll.
- [ ] **No in-flight VM creates**: `weft vm list --state=creating` returns
  empty. A create that straddles a restart will retry, but the cleanest
  roll is on a quiescent control plane.
- [ ] **Fresh etcd snapshot taken** — see [etcd-backup.md](etcd-backup.md).
  This is your rollback floor if the new agent corrupts state.
- [ ] **CHANGELOG read** for the target version — confirm it's a
  minor-version bump (wire-compatible) or, if it's a major bump, that
  the staged migration runbook for that release has been followed up to
  this point.
- [ ] **Image pulled but not yet activated** on all three hosts (warming
  the local cache shortens the per-host window):
  ```
  ssh host-1 docker pull ghcr.io/openweft/weft:vX.Y.Z   # or oras pull
  ssh host-2 docker pull ghcr.io/openweft/weft:vX.Y.Z
  ssh host-3 docker pull ghcr.io/openweft/weft:vX.Y.Z
  ```

## 3. The roll

Three phases, one host each. Default soak between phases: **10 minutes**.

### Phase 1 — canary (host-1)

The canary is the load-bearing step. If the new code panics on real
state, the other two hosts keep serving and you roll back from one host,
not three.

1. **Drain host-1** (see [Cordon implementation note](#cordon-implementation-note)
   for what "drain" means in v0.1.0).
2. **Install the new binary**:
   ```
   ssh host-1 'install -m0755 /var/cache/weft/weft-vX.Y.Z /usr/local/bin/weft'
   ```
   (Or whatever mechanism your image-pull step produced — the goal is
   `/usr/local/bin/weft` pointing at the new build.)
3. **Restart the unit**:
   ```
   ssh host-1 systemctl restart weft-agent.service
   ssh host-1 systemctl status  weft-agent.service --no-pager
   ssh host-1 journalctl -u weft-agent.service -n 100 --no-pager
   ```
4. **Confirm rejoin**:
   ```
   etcdctl --endpoints=$E1,$E2,$E3 endpoint status --write-out=table
   weft hosts list           # host-1 should be 'ready' within ~5s
   ```
5. **Soak 10 minutes** while watching the metrics in §4. If clean,
   proceed.

### Phase 2 — progressive (host-2)

Identical procedure on host-2. Quorum at this point: host-1 (new) +
host-3 (old) = 2/3, so host-2 absent is fine.

### Phase 3 — final (host-3)

Identical procedure on host-3. Quorum at this point: host-1 (new) +
host-2 (new) = 2/3.

At every intermediate moment **at least 2 of 3 agents are running
compatible versions** — that's the invariant that keeps the roll
non-disruptive.

## 4. What to watch during a roll

Open these before Phase 1 and keep them open through Phase 3.

| Signal | Where | Hard-stop threshold |
|---|---|---|
| gRPC error rate | Grafana panel **weft-agent / RPC errors** ([grafana/README.md](grafana/README.md), dashboard `grafana/weft-agent.json`) | `>2x` 24-hour baseline |
| etcd leader changes | `etcdctl endpoint status` (`IS LEADER` column) | More than one flip per phase |
| etcd alarms | `etcdctl alarm list` | Any non-empty output |
| Panics / fatal | `journalctl -u weft-agent.service -p err -f` on each host | Any `panic:` line |
| Audit log throughput | Grafana **audit log events/s** panel | Drops to 0 on an upgraded host |
| Host roster | `weft hosts list` | Any host stuck `not_ready` >30s post-restart |

If any **hard-stop** trips: don't proceed to the next phase. Go to §5.

For dashboard provisioning + the full metric catalogue, see
[observability.md](observability.md).

## 5. Roll-back

Roll-back is the same shape as roll-forward, one host at a time.

1. `systemctl stop weft-agent.service` on the affected host.
2. Re-install the prior binary (pin the previous tag — keep
   `/var/cache/weft/weft-v0.1.0` around for exactly this reason).
3. `systemctl start weft-agent.service` and confirm rejoin.
4. Proceed to the next host **only** if the cluster is stable.

**Destructive case** — if the failed version performed an **etcd schema
migration** (the CHANGELOG for that release will call this out under a
`### Schema migration` heading), simple binary roll-back is **not safe**.
The on-disk etcd keys are now in the new shape and the old binary will
mis-parse them. In that case:

- Stop all three agents.
- Restore the pre-upgrade snapshot per the *Restore* section of
  [etcd-backup.md](etcd-backup.md).
- If the restore fails or state has diverged too far, escalate per
  [disaster-recovery.md](disaster-recovery.md).

## Cordon implementation note

The procedure above says "drain host-1". The **intended** API surface is
a per-host `host.cordoned` flag that the scheduler honours — when set,
the host receives no new VM placement, while existing VMs keep running.

> **Available since v0.2.0.** The `host.cordoned` flag is now wired
> through `weft host cordon <host>` / `weft host uncordon <host>` and
> the scheduler drops cordoned hosts from candidate sets for new
> placements. Existing VMs keep running.

**Workaround for v0.1.0 today** — two options:

1. **Manual cordon via weight**: edit `cluster.hcl`, set the host's
   scheduling `weight = 0`, and re-run `weft up -f cluster.hcl --apply`.
   The planner will stop placing new VMs on that host. After the
   upgrade, restore the original weight and re-apply.
2. **Accept in-flight churn**: skip the drain step entirely. New VM
   creates that land on the host during its ~5–10s restart window will
   retry once the agent comes back. Acceptable for clusters with low
   create rate, **not** acceptable during a large fan-out (e.g. mid-deploy).

## 6. Cross-version compatibility

See [CHANGELOG.md](../../CHANGELOG.md) for the per-release notes.

**Policy:**

- **Minor-version bumps** (`v0.1.0 → v0.1.x`, eventually `v1.1.0 → v1.2.0`)
  are **guaranteed wire-compatible** in both directions. Mixed-version
  clusters during a roll are explicitly supported.
- **Major-version bumps** (`v0.x → v1.0`, `v1.x → v2.0`) require a
  **staged migration runbook published per release**. Do not start a
  major-version roll without reading that runbook first; it will
  typically prescribe an intermediate version that knows how to speak
  both wire formats.

## 7. What this runbook does NOT cover

- **Kernel-image upgrades** for guest microVMs — the `weft-microvm-kernel`
  OCI artifact is pinned via `microvm { kernel_ref = "..." }` in
  `cluster.hcl`. Bump the ref and re-apply; agents pull on next pod-init
  build. Separate lifecycle from the agent.
- **Driver-plugin upgrades** — each hypervisor driver (`weft-driver-vz`,
  `weft-driver-qemu`, `weft-block`, …) is versioned independently and
  pulled via the `drivers {}` block in `cluster.hcl`. See the per-driver
  release notes; agent restart picks up the new digest on next plugin
  launch.
- **Etcd version upgrades** — out of scope. Follow upstream etcd's
  rolling-upgrade procedure; don't bounce weft-agent and etcd in the
  same window.
- **Failover drills** — see [ha-failover.md](ha-failover.md). Run
  quarterly, *separately* from upgrades.
- **Image signature verification** — `cosign-verify.md` (sibling doc,
  not yet merged). Until then, verify by digest pin in `cluster.hcl`.
