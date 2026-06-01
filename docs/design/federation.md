# Multi-cluster federation — design

Status : **design, not implemented** (target : federation-lite scaffolding
in `v0.2`, full primitives in `v0.3`). See
[`docs/operations/federation.md`](../operations/federation.md) for the
operator-facing summary.

## 1. Problem statement

A weft cluster spans 1..N hosts inside one geographic group — a "metro"
or "site". Within that group, latency between hosts stays in the
microsecond–millisecond range, etcd consensus is cheap, and the
operator's blast radius is bounded by one DC's power/network domain.
Federation kicks in when that bound stops being enough :

- **Geographic latency.** A single etcd quorum cannot straddle
  oceans. Two clusters in `eu-west` and `us-east` is the cheap way to
  serve both populations close to home without paying a 90 ms RTT on
  every control-plane write.
- **Regulatory boundaries.** GDPR / data-residency rules want tenant
  data physically inside one jurisdiction. The cleanest enforcement is
  "this tenant lives in cluster `eu-paris`, full stop", which requires
  global tenant→cluster routing.
- **Blast-radius isolation.** A bad upgrade, a fat-finger `etcdctl`,
  or a runaway control loop should not be able to break customers in
  another DC. Cluster boundaries become the unit of failure.
- **Multi-cloud / hybrid.** One cluster on bare-metal in colo, one on
  Hetzner, one on a customer's on-prem hosts. Same operator, same
  tenants, three independent control planes.

## 2. Constraints

- **Single-cluster deploys must stay first-class.** No required
  dependency on a federation control plane. The 1-host dev cluster
  and the 3-DC production cluster from
  [`docs/getting-started`](../getting-started/) keep working with
  zero federation config.
- **No mandatory new daemon.** The cheap path (federation-lite, §3)
  reuses `weft-agent` ; no extra process for operators who already
  run weft on three hosts and want to glue two such trios together.
- **Backwards-compatible CLI.** Federation primitives are namespaced
  under `weft federation <verb>` ; existing `weft host`, `weft vm`,
  `weft volume` semantics are unchanged.

## 3. Two federation models considered

### 3a. Federation lite — gossip + DNS

Each cluster's `weft-agent` (in its leader role) exposes a read-only
`/cluster-info` endpoint that returns the signed cluster manifest
(§5). Discovery is bootstrapped via DNS SRV records of the form
`_weft._tcp.example.com → <weft-agent endpoints>` ; once one peer is
known, the manifest carries the rest. Each cluster keeps its own
etcd ; **there is no shared global state**.

Resource-placement queries (`weft federation place …`) are answered
locally by the receiving cluster, which knows its own capacity and
hints at peers using their last pulled `/cluster-info` snapshot. The
placement output is a *recommendation*, not a binding lease — the
caller picks a target cluster and submits the workload there
directly.

Pros : no new daemon, no new failure mode, no global etcd, operator
can opt-in per-cluster. Cons : recommendations are eventually
consistent (last-poll-old), no global scheduler, no cross-cluster
preemption.

### 3b. Federation full — `weft-federation` control plane

A new stateful component `weft-federation` runs as a separate service
(its own repo, its own deploy story). It watches each member cluster's
etcd via the existing pull/watch surface, maintains a *global view*
(VMs, volumes, hosts, capacity) in its own store, and schedules
cross-cluster placements via signed leases that member clusters honor.

Pros : single global queue, cross-cluster preemption is possible,
global quota enforcement. Cons : significantly more complex —
another HA daemon, another quorum to babysit, another upgrade path,
another security boundary. Effectively a meta-cluster control plane.

## 4. Recommendation

**Go with federation-lite for v0.2.** The operator's tradeoff is
explicit : you get cross-cluster awareness without paying for a global
control plane you may not need. The two clusters that motivated the
feature (regulatory routing, multi-cloud) both work fine with
recommendation-only placement ; truly global scheduling can be added
later as an opt-in `weft-federation` deployment without breaking the
lite story.

Federation-full is **not** rejected — it's deferred. The lite design
is a strict subset : the manifest shape, the join/leave verbs, the
trust model and the `/cluster-info` endpoint are reused as-is when
the full control plane is introduced.

## 5. Data model

```go
// Cluster is one federation member. Carried inside FederationManifest.
type Cluster struct {
    Name             string   // globally unique within a federation
    Region           string   // free-form, e.g. "eu-west-3", "us-east-1"
    Datacenters      []string // 1..N DCs in this cluster
    Weight           int      // placement bias, default 100
    PublicEndpoints  []string // weft-agent leader URLs (https://...)
    CertificateBytes []byte   // PEM cert of the cluster's CA (TLS pin)
}

// FederationManifest is the signed source of truth for the
// federation membership list. Each cluster keeps its own copy ;
// version is monotonic and conflicts are resolved
// highest-version-wins among peers signed by a quorum of members.
type FederationManifest struct {
    Name    string    // operator-chosen, e.g. "acme-global"
    Version uint64    // monotonic
    Members []Cluster // 1..N
}
```

Signing : cosign keyless (GitHub OIDC) if the manifest is published
from a public GitHub repo, otherwise a long-lived admin signing key
held by the federation owner. Signature verification reuses the same
Sigstore-pinning machinery documented in
[`docs/operations/cosign-verify.md`](../operations/cosign-verify.md).

## 6. Federation primitives (CLI)

All under `weft federation <verb>` ; all idempotent ; all admin-only.

- `weft federation join <peer-url> --token <bootstrap-token>` —
  one-shot bootstrap : exchanges manifests, validates the peer's
  signature against the configured trust root, adds the peer to the
  local registry. The token is short-lived (15 min) and minted by
  the peer cluster's admin.
- `weft federation leave <name>` — remove a member locally and bump
  the local manifest version. Other peers learn via pull.
- `weft federation list` — show known clusters + last-seen + manifest
  version + signature status. Operator's primary "is the federation
  healthy" view.
- `weft federation place --tenant <t> --constraints <c>` — returns a
  recommendation : which member cluster best satisfies the given
  constraints (region match, free capacity, GPU model availability,
  tenant residency). Output is structured (JSON), purely advisory ;
  the operator or a higher-level controller decides what to do with
  it.

## 7. Resource scoping

What replicates across federation members vs. what stays local :

| Resource             | Federated ?                                | Notes |
|----------------------|--------------------------------------------|-------|
| Tenant manifests     | Yes — intent only                          | each cluster materialises tenants locally. |
| Network policies     | Yes — intent only                          | each cluster's weft-network reconciles its own dataplane. |
| Image catalogues     | Yes — by ref (OCI)                         | pulls still happen per-cluster. |
| Scheduling rules     | Per-cluster                                | rule semantics are cluster-local ; cross-cluster rules need v0.3. |
| VMs / microVMs       | Per-cluster                                | a VM lives in one cluster, period. |
| Volumes              | Per-cluster                                | cross-cluster replication is out of scope (§10). |
| Hosts                | Per-cluster                                | hosts never appear in another cluster's inventory. |
| Audit logs           | Per-cluster                                | each cluster's `auditlog/` sink is independent. |

The rule of thumb : **identity & policy federates ; physical
resources don't**. This matches the federation-lite "no global state"
constraint — federated objects are small, slow-changing, and
signature-verifiable as a manifest ; physical resources are large,
churny, and best kept inside their cluster's etcd.

## 8. Trust model

Each member trusts :

1. The federation manifest's signing key (cosign identity or
   admin-held key). Verified on every pull of `/cluster-info`.
2. Its own admin OIDC for local `weft federation …` commands —
   identical RBAC story to the rest of the CLI, see
   [`docs/operations/rbac.md`](../operations/rbac.md).
3. Each peer's TLS cert as pinned by `Cluster.CertificateBytes` in
   the manifest. A peer rotating its cert needs a manifest version
   bump.

Joining a federation requires admin on **both** sides : peer mints a
bootstrap token, joiner runs `weft federation join` with it ; neither
side trusts the other on bare URL alone. This rules out a rogue
cluster forcing itself in by spoofing a manifest.

Misbehaving members are evicted via a manifest-version bump signed by
a quorum of the remaining members. Quorum is `⌈(N-1)/2⌉ + 1` where N
is the membership pre-eviction — the same shape as etcd's removal
math, applied to manifest signatures instead of Raft commits.

## 9. Pull-model alignment

Per the `openweft_pull_model` memory : **cross-daemon flows are
pull/reconcile, not push.** Federation events fit cleanly :

- A cluster discovers peer state by **polling** `/cluster-info` on a
  configured cadence (default : 30 s, jittered). No member pushes
  state to another member's etcd.
- A SchedulingRule with `federated: true` is reconciled by the local
  cluster ; its presence is a **hint** that the rule's selector is
  expected to be evaluated against the federation-wide view, not a
  push to other clusters.
- `weft federation place` is a synchronous read against locally
  cached peer state — the freshness bound is the polling interval.

Per `openweft_nominal_binding` : the nominal vs. selector dance
applies to federation-level resource binding too. A workload that
names a target cluster explicitly (`spec.cluster: "eu-paris"`) is a
**nominal binding** — the placer treats it as authoritative even
if the cluster's selector wouldn't have matched. Selectors only
fire when the nominal field is unset. This matches the k8s
`PersistentVolumeClaim.volumeName` vs. `selector` rule and is
intentional carry-over.

## 10. Out of scope

This design **does not** cover :

- **Cross-cluster volume replication.** Belongs to a Longhorn /
  `weft-block` follow-up. The per-cluster storage boundary in §7
  is deliberate ; replication-across-WAN is a different problem with
  different consistency knobs.
- **Cross-cluster VM live migration.** Multi-year R&D ; needs page
  fault handling over WAN, shared block visibility, and at least
  one cooperating hypervisor pair. Out of scope, intentionally.
- **Shared block storage across clusters.** The cluster is the
  storage boundary. Shared NFS / S3 catalogues are fine (they're
  external) ; pretending two clusters share an LVM VG is not.
- **Global single-pane-of-glass UI.** `weft-webui` may grow a
  federation-aware mode later ; v0.2 ships with per-cluster UIs and
  a CLI that knows about peers.
