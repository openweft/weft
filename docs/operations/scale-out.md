# Adding a hypervisor to an existing cluster

Operator runbook for growing a running weft cluster past its day-0 host
count. Two supported paths : convergent (`weft up --apply` on an edited
`cluster.hcl`, recommended) and explicit (`weft host register` against a
running agent, for ad-hoc compute). Read the convergent path first —
that's the one that keeps your HCL as source of truth and survives
operator turnover.

## When to scale out

Three motivations dominate :

- **Capacity** — CPU / RAM / disk headroom is gone, scheduler is
  packing VMs onto fewer hosts than your placement rules want. Add a
  host in an existing AZ to absorb load.
- **AZ resilience past 3 hosts** — moving from 3 to 5 (or 5 to 7)
  voting members lifts the failure tolerance from "1 DC down" to "2
  DCs down". Mandatory if your SLA actually requires 2-AZ tolerance ;
  optional otherwise (a 3-DC cluster already tolerates 1-AZ loss, see
  [ha-failover.md](ha-failover.md)).
- **GPU-only host pools** — a new tier of hardware (H200, RTX 6000 Ada)
  joined alongside the general-purpose pool, labelled so that
  GPU-bound SchedulingRules pin to it. See
  [gpu-scheduling.md](gpu-scheduling.md) for the label convention.

If the goal is just to swap one host for a new one (same AZ, same
role), follow [drain-remove-host.md](drain-remove-host.md) on the
outgoing host first, then this runbook on the incoming one.

## Two paths

| Path | When | Trade-off |
|---|---|---|
| `weft up --apply` on edited `cluster.hcl` | **Default.** Permanent additions, etcd voting members, anything that should survive operator turnover. | Slower (full convergent pass). Requires HCL access. |
| `weft host register` against running agent | Ad-hoc compute capacity, transient bursts, GPU pool that lives outside the HCL truth. | Host is not in HCL — re-running `weft up --apply` won't undo the registration, but won't push config updates to the host either. |

## Path A — convergent (`weft up --apply`)

### 1. Provision the new host

Exactly like the day-0 procedure : drop
`examples/cloud-init/debian-host.yaml` into the install seed, ensure
the firewall opens etcd 2379/2380, mesh WireGuard UDP, gRPC 9090,
metrics 9101. Agent will come up in the `awaiting /etc/weft/weft.hcl`
state — that's expected.

Verify from the operator station :

```sh
ssh admin@10.0.0.14 systemctl is-active weft.service
# expected: activating (waiting for config)
```

### 2. Add a `host "h4"` block to `cluster.hcl`

```hcl
cluster "prod" {
  # ... existing overlay, agent_config, etc.

  host "h1" { address = "10.0.0.11" dc = "dc1" hypervisor = "qemu" ssh { user = "admin" } }
  host "h2" { address = "10.0.0.12" dc = "dc2" hypervisor = "qemu" ssh { user = "admin" } }
  host "h3" { address = "10.0.0.13" dc = "dc3" hypervisor = "qemu" ssh { user = "admin" } }
  host "h4" { address = "10.0.0.14" dc = "dc4" hypervisor = "qemu" ssh { user = "admin" } }
}
```

Pick `dc` deliberately — re-using an existing AZ adds capacity to that
AZ ; minting a new one (`dc4`) lifts the AZ count, which has knock-on
effects on placement rules with `az: different` constraints and on the
etcd member count (see pitfalls below).

### 3. Re-run `weft up --apply`

```sh
weft up -f cluster.hcl --apply
```

What the planner does (delta against the running cluster) :

- pushes `/etc/weft/weft.hcl` to `h4`
- starts `weft.service` on `h4`
- joins `h4` into the WireGuard mesh (adds one peer entry on h1/h2/h3,
  one full mesh config on h4)
- registers `h4` in the host registry
- **does not** automatically grow etcd voting membership — that's an
  explicit operator decision (see pitfalls)

Expected output :

```
[1/4] h1: no changes
[2/4] h2: no changes
[3/4] h3: mesh peer added (h4)
[4/4] h4: weft.hcl pushed, agent started, joined cluster
cluster prod ready (4 hosts, quorum: 3/3, proxy: enabled)
```

### 4. Verify

```sh
weft host ls
# expected: 4 rows, h4 state=Running, az=dc4 or your chosen AZ

etcdctl --endpoints=$E1,$E2,$E3 member list
# expected: still 3 members (h4 is NOT a voting member yet, by design)

weft cluster status
# expected: hosts=4/4, quorum=3/3, drivers ready on h4
```

If `h4` shows up as `state=Down` for more than 90s after the apply,
check the agent log on the host (`journalctl -u weft.service`) — most
common cause is a wrong SSH key (see pitfalls).

## Path B — explicit `weft host register`

Useful for ad-hoc compute hosts that should not live in the HCL truth
(short-lived burst capacity, GPU pool driven by a separate workflow,
test hosts).

### 1. Bring up the agent on the target

Boot the host with the same cloud-init seed as Path A, then push a
minimal `weft.hcl` (no `host {}` blocks needed — the host registers
itself). Start the agent :

```sh
ssh admin@10.0.0.14 'sudo systemctl start weft.service'
```

### 2. Register from the operator station

```sh
weft host register \
    --hostname=h4 \
    --az=dc4 \
    --rack=r1 \
    --hypervisor=qemu-kvm \
    --architecture=arm64 \
    --endpoint=10.9.0.14:9090 \
    --network-types=nat,mesh \
    --volume-backends=file,longhorn \
    --labels=role=compute,tier=burst
```

The command is idempotent on `--uuid` — pass a stable UUID to make
re-registers safe across host reboots.

### Caveats

- The host is **not part of `weft up --apply`'s convergent set**. If
  you later re-run `weft up --apply`, it won't undo this registration,
  but it also won't push config updates (new image refs, OIDC issuer
  rotation, etc.) — you'd need to either add the host to the HCL
  retroactively or run the config push manually.
- The host's mesh entry exists only because the local agent built it
  at startup — there's no convergent re-derivation. If the cluster
  overlay subnet ever changes, hand-registered hosts won't follow.
- `etcdctl member list` is unaffected ; explicit registration never
  touches etcd voting membership.

## Common pitfalls

**Forgetting to bump etcd member count when AZ count rises.** A 3-DC
cluster has 3 voting etcd members. If you scale to 4 AZs and want
real 2-AZ failure tolerance, grow etcd to 5 members first
(`etcdctl member add`), then add the 4th and 5th hosts. The valid
member counts are **3 → 5 → 7** ; even numbers split the vote and never
**>7** (Raft latency degrades fast past 7). Reference :
[etcd FAQ on cluster size](https://etcd.io/docs/v3.5/faq/#why-an-odd-number-of-cluster-members).

**Architecture mismatch.** Mixing amd64 and arm64 hosts is supported
but the agent needs to know — pass `--architecture` explicitly on
`weft host register`, or set it in the HCL `host {}` block. Without
it, the scheduler may try to place an arm64 image on an amd64 host
and the QEMU driver returns `EINVAL`.

**SSH key on the new host.** Path A pushes `/etc/weft/weft.hcl` over
SSH using the key declared under `ssh {}` in `cluster.hcl`. If that
key isn't in the new host's `authorized_keys`, the apply fails at
step "push weft.hcl to h4". Solution : drop the same key into the
cloud-init seed of the new host, or pass it via `ssh_authorized_keys`
in `examples/cloud-init/debian-host.yaml`.

**Mesh CIDR exhaustion.** The default `overlay.subnet = "10.9.0.0/24"`
gives 254 host slots. That's plenty for most clusters but if you're
heading past ~200 hosts, plan for `/22` (1022 slots) on day-0 — you
can't widen the subnet without re-keying every peer. Memory ref :
[`wireguard_replaces_vxlan`](../../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/wireguard_replaces_vxlan.md).

**Mixing hypervisor kinds in one AZ.** A single AZ with both
`qemu-kvm` and `apple-vz` hosts is supported by the scheduler but
breaks SchedulingRules that nominally bind to "any host in dc1".
Cleanest pattern : one hypervisor kind per AZ, labels for finer
distinctions.

## Verification (gold standard)

The chaos test
[`weft-chaos/scripts/chaos-3dc-kill-restart.sh`](../../../weft-chaos/scripts/chaos-3dc-kill-restart.sh)
exercises the per-host lifecycle (register → schedule → kill →
restart → recover) and is the right harness to validate that the new
host is fully integrated. Run it against the now-4-host cluster ; a
green run means placement, mesh, etcd quorum, and proxy plane all
recognise the new member.

If the script flags an anomaly, file an issue with the
`weft cluster status -o json` snapshot taken right after the failure.

## Next steps

Once the cluster is grown :

- Re-take an etcd snapshot — [etcd-backup.md](etcd-backup.md). The
  baseline you had includes only 3 hosts ; a fresh snapshot reflects
  the current membership.
- Re-run the HA drill — [ha-failover.md](ha-failover.md). 4-host
  topology has different failure semantics than 3-host (DC-1 down
  leaves 3 hosts alive, but if etcd is still 3-member you've gained
  no quorum margin).
- If this is the third or fourth host in a previously-1-host cluster,
  follow the "1 → 3" extension notes in
  [getting-started/production-3host.md](../getting-started/production-3host.md)
  for the etcd grow path.
