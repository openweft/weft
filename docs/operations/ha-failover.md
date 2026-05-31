# HA failover runbook (3-DC cluster)

Validates the 3-DC weft cluster's tolerance to losing a single DC. The
test simulates a hard failure (host poweroff / network partition / etcd
process kill) on one DC's host and walks through the expected behaviour
plus the recovery procedure when the DC comes back.

Run this every quarter as a fire drill — the cost of *finding out* HA
doesn't work during a real incident is much higher than the 20 minutes
this takes against a lab cluster.

## Prerequisites

- 3-DC cluster brought up via `weft up -f cluster.hcl --apply`, all
  three hosts reachable, all infra microVMs (etcd × 3, coredns × 3,
  dex × 3, nats × 3, zot × 3) registered. Verify with:
  ```
  weft hosts list
  weft microvm list
  ```
  Should show 3 hosts active and 15/15 microVMs.
- `etcdctl` installed locally. The 3 etcd endpoints from
  `cluster.hcl`'s `host {}` blocks reachable from your workstation.
- Optional but recommended: `weft webui` open in another tab — the
  topology view tracks state changes live.

## What we're testing

| Concern | Expected behaviour on DC-1 kill |
|---|---|
| etcd quorum | DC-2 + DC-3 keep majority (2/3), cluster stays writable |
| coredns | DC-2 + DC-3 instances continue answering queries |
| dex (OIDC) | Same — token validation continues against DC-2 / DC-3 |
| nats | JetStream R=3 streams keep 2 replicas, no data loss |
| zot (OCI registry) | Pulls continue via DC-2 / DC-3; new pushes need quorum |
| weft control plane | Survives — schedulers on DC-2 / DC-3 handle the host roster |
| Existing user VMs on DC-1 | Frozen (host is down). Recovery = boot DC-1 back, VMs resume |
| New VM placement | DC-1 marked `down`, placement rules with `dc: any` succeed on DC-2/3 |

## Procedure

### Step 1 — establish baseline

```bash
# Quorum check before the kill
etcdctl --endpoints=$DC1_ETCD,$DC2_ETCD,$DC3_ETCD endpoint status -w table

# Snapshot the host registry — useful to diff after recovery
weft hosts list -o json > /tmp/hosts-baseline.json
weft microvm list -o json > /tmp/vms-baseline.json
```

Expected: 3 endpoints alive, one of them flagged `IS LEADER`. Snapshot
files capture today's state.

### Step 2 — kill DC-1

Pick your weapon based on the failure mode you're modelling:

**Hard poweroff** (closest to real datacenter loss):

```bash
ssh root@dc1-r1-h1 'systemctl poweroff -f'
# or, if the host is a VM: virsh destroy dc1-r1-h1 / qemu monitor 'quit'
```

**Network partition** (split-brain test — keep the host up but
unreachable):

```bash
ssh root@dc1-r1-h1 'iptables -A INPUT -p tcp --dport 2379 -j DROP'
ssh root@dc1-r1-h1 'iptables -A INPUT -p tcp --dport 51820 -j DROP'
```

**Just etcd** (narrowest test, isolates the quorum logic):

```bash
ssh root@dc1-r1-h1 'systemctl stop weft-microvm-etcd-1'
```

### Step 3 — observe

Within 30s the surviving DCs should reach quorum without DC-1:

```bash
etcdctl --endpoints=$DC2_ETCD,$DC3_ETCD endpoint status -w table
```

Expected: 2 endpoints alive, one is now `IS LEADER` (re-elected). The
DC-1 endpoint times out — that's normal.

Check the host registry:

```bash
weft hosts list
```

Expected: `dc1-r1-h1` flipped to `state=down` within
`HeartbeatInterval × 3` (~90s default). The other two hosts stay
`active`.

Run a placement-rules check by spinning up a new VM. It should land on
DC-2 or DC-3:

```bash
weft microvm register --name=failover-test \
    --image=ghcr.io/openweft/weft-test-fixture:latest \
    --placement="dc:any"
```

Expected: succeeds, `dc1-r1-h1` excluded automatically.

### Step 4 — recovery

Bring DC-1 back:

**From poweroff**: power on the host. weft-agent's systemd unit comes
up, re-registers under the existing UUID (per memory `weft up gaps`
`RegisterMicroVM` is idempotent), etcd member rejoins:

```bash
ssh root@dc1-r1-h1 'systemctl start weft-agent'
```

**From network partition**: drop the firewall rules:

```bash
ssh root@dc1-r1-h1 'iptables -F INPUT'
```

**From etcd-only stop**: restart the microVM:

```bash
ssh root@dc1-r1-h1 'systemctl start weft-microvm-etcd-1'
```

Within ~60s, etcd should re-join the quorum:

```bash
etcdctl --endpoints=$DC1_ETCD,$DC2_ETCD,$DC3_ETCD endpoint status -w table
```

Expected: 3 endpoints alive again, one is leader (may or may not be
DC-1's — etcd doesn't move leadership back automatically, that's by
design).

Verify the host registry healed:

```bash
weft hosts list
```

Expected: `dc1-r1-h1` back to `state=active`. `last_seen_at` recent.

Diff vs baseline:

```bash
weft hosts list -o json | jq -S . > /tmp/hosts-after.json
diff <(jq -S . /tmp/hosts-baseline.json) /tmp/hosts-after.json
```

Expected diff: only `last_seen_at_unix_ns` deltas. Anything else
(missing host, changed UUID, lost label) is a regression — file it.

### Step 5 — clean up the failover test VM

```bash
weft microvm delete --name=failover-test
```

## Pitfalls

**Quorum lost after killing two DCs**: tempting to "really stress test"
by killing DC-1 + DC-2 simultaneously. Don't — etcd needs a majority
(2/3) for writes. A single-DC test validates the design; a two-DC test
just confirms quorum is needed, which is already documented.

**Stale leases**: if DC-1's weft-agent registered VMs with
`HeartbeatInterval: 5*time.Minute` (the default is 30s, but operators
sometimes raise it for noisy logs), the registry may keep `dc1-r1-h1`
in `active` for the full interval. Pin a low value during the drill.

**Proxy plane**: if the `proxy {}` block is enabled and points at all
three etcd endpoints in `storage.endpoints`, Caddy survives one DC
loss seamlessly. If the operator listed only DC-1's endpoint (a config
mistake), all certs become unwritable when DC-1 dies. The runbook
implicitly tests this — Caddy should keep serving from the existing
cert cache and emit a single warning per stalled write. If it does
*more* than warn, that's the misconfiguration to fix.

## What this test does NOT cover

- **Two-DC outage** — out of scope, see "Pitfalls" above.
- **Bit rot / silent data corruption** — etcd has its own
  consistency checks; if those are off the cluster has bigger
  problems than a failover drill catches.
- **Backup / restore** — that's [`etcd-backup.md`](etcd-backup.md). HA
  is about *no data loss during the outage*, backup is about
  *recoverable state after total cluster loss*.
- **Caddy cert reload under DC-1-down** — covered above in passing;
  for a deeper test, force a cert renewal during the outage by
  setting `tls.automation.policies[0].renewal_window_ratio` to 1.0
  temporarily.

## Logging the drill

Keep a quarterly log under `docs/operations/ha-drills/` — date, who
ran it, the failure mode, how long recovery took, anomalies found. The
log is the artefact that lets a future SRE see "this was tested in Q1
2026, took 4min to recover, no regressions". Three drills with no
regressions is the threshold to mark HA as "validated" in
operator-facing docs (vs. "designed").
