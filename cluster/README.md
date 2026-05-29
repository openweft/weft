# cluster — `weft up`

Day-0 bring-up for a weft deployment: read a `cluster.hcl` describing the
hypervisor hosts and converge the control plane + infra micro-VMs to it. One
command, two shapes, in-place growth:

- **1 host** → single-node (every infra service runs as one replica).
- **3 hosts** → 3-DC cluster (one etcd/nats/… replica per DC, WireGuard mesh,
  quorum).
- **1 → 3** → re-run after adding hosts to the HCL; only the delta is applied
  (join the new hosts, grow etcd/nats quorum, place the missing replicas,
  extend the mesh).

This package is the **model + planner** (pure, unit-tested, no host/hypervisor
dependency). `cmd/weft/up.go` is the command; execution runs over SSH.

## `cluster.hcl`

```hcl
cluster "prod" {
  overlay { subnet = "10.9.0.0/24" }   # WireGuard overlay shared by hosts + infra VMs

  host "a" {
    address    = "192.0.2.1"           # underlay-reachable IP/host (SSH + WG endpoint)
    dc         = "dc1"                  # AZ label; defaults to the host id
    hypervisor = "qemu"                # "" (auto) | "vz" | "qemu"
    ssh {
      user = "ops"
      key  = "/home/ops/.ssh/id_ed25519"
    }
  }
  host "b" { address = "192.0.2.2"  dc = "dc2"  hypervisor = "qemu" }
  host "c" { address = "192.0.2.3"  dc = "dc3"  hypervisor = "qemu" }

  infra { services = ["etcd", "nats", "dex", "zot", "coredns"] }  # default: all under infra/
}
```

Validation: exactly **1 or 3** hosts; unique host ids and DCs (one host per DC);
a parseable overlay subnet; `hypervisor ∈ {"", "vz", "qemu"}`. The `infra.services`
subset must be **dependency-closed** (e.g. selecting `nats` requires `dex`/`etcd`)
— omit the block to deploy every plan under `infra/`.

## Topology & convergence

`Build(cluster, infraDAG, observed)` computes **desired − observed → ordered
actions**. Desired placement:

- replica count per service = `1` on a single host, else `min(declared, hosts)`
  (declared = the plan's `placement.count` or the number of `static_ip`s);
- replica *i* lands on host *i* (i.e. DC *i*); singletons on the seed.

Actions (in order): `ensure-host` (new hosts) → `mesh-sync` (if membership grew)
→ `place-replica` (per service, dependency-sorted, skipping anything already
correctly placed) → `grow-quorum` (etcd/nats that gained members; skipped on a
fresh bootstrap, which uses `initial-cluster-state=new`).

Because the plan is `desired − observed`, the **same command** bootstraps, is a
no-op when converged, and applies only the delta when you grow 1 → 3 — that is
the "extend a host to a cluster" path, not a separate code path.

## SSH access model

Chosen model: `weft up` reaches each hypervisor **over SSH**, both to install /
start its agent and to drive the per-host deploy. The first host is the
**seed** (control plane); the others join it.

`RenderSSH` turns the plan into per-host command sequences (the seed anchors
cross-host steps):

```
ssh ops@192.0.2.1
    weft agent --server --hypervisor=qemu        # seed control-plane
    # mesh: publish overlay peer set to [a,b,c]  (wgcoord + mesh.PublishAll)
    weft infra deploy etcd   # replica 1 (dc=dc1)
    …
ssh root@192.0.2.2
    weft agent --client --control-plane=192.0.2.1 --hypervisor=qemu  # join seed
    weft infra deploy etcd   # replica 2 (dc=dc2)
    …
```

Per-host SSH target resolves from the `ssh { user, key }` block (user defaults
to `root`). Lines starting with `#` are operator/control-plane coordination
notes (mesh keys, quorum growth), logged rather than run remotely.

- `weft up -f cluster.hcl` — print the convergence plan (dry-run).
- `weft up -f cluster.hcl --ssh` — also print the per-host SSH plan.
- `weft up -f cluster.hcl --apply` — execute it over SSH (needs the hosts
  reachable with the configured keys).

> Auth uses the per-host private key; host-key verification is skipped (dev) —
> production must pin `known_hosts`.

## Layering

`weft up` is the **operator-facing, cluster-aware, convergent** entry point. It
composes the existing per-host primitive rather than replacing it:

```
weft up   (cluster.hcl · single|3-DC · convergent · SSH-driven)
  └─ per host (over SSH) ─> weft agent (seed --server / others --client)
                            weft infra bootstrap|deploy   (per-host primitive, kept)
                              └─ infra.{LoadAllPlans, TopologicalSort, deployPlan}  (engine)
  └─ cross-host ─> overlay mesh (wgcoord.MeshPeers + mesh.PublishAll)
                   etcd/nats quorum (initial-cluster / member-add)
```

`weft infra bootstrap` stays as the in-process per-host primitive (it runs
before any gRPC is reachable — exactly what the seed needs) and as a dev
escape hatch.

## Status

Planner + SSH rendering: implemented and unit-tested (single / 3-DC / extend /
converged; target resolution; per-host command rendering). `--apply` performs
the live SSH run.

Remaining for a fully-closed `--apply` (needs real hosts to validate):

1. **Observed-state query** — today the plan is computed against an empty state
   (bootstrap). Feeding the running control plane's actual hosts + placements
   as `observed` turns it into the true extend/grow delta.
2. **Per-replica infra deploy** — `weft infra deploy <svc>` currently deploys a
   service locally; cluster placement needs a per-replica/per-host target flag.
3. **Quorum growth + mesh keys** — wire `grow-quorum` to etcd member-add / nats
   routes, and `mesh-sync` to wgcoord key minting + `mesh.PublishAll`.
4. **Agent install** — staging the `weft` binary on a host before `weft agent`
   (scp/push) under the SSH model.
