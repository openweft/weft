# Multi-cluster federation

> **Status : design only — not implemented in v0.1.0.**
> The shape below is what `weft federation …` will look like once it
> ships. Today the verbs are unrecognised and the data structures live
> only in `federation/manifest.go` as a stub.

A weft cluster spans 1..N hosts in one site. Federation lets you treat
M of those clusters as a single addressable surface for tenant-level
policy and resource-placement *recommendations* — without sharing
etcd, without a new mandatory daemon, and without giving up the
single-cluster deploy story.

See [`docs/design/federation.md`](../design/federation.md) for the full
design rationale, the lite-vs-full tradeoff, the trust model and the
out-of-scope list.

## Timeline

| Release | What lands |
|---|---|
| **v0.1.0** | Design only (this page + the design doc + a stub package). |
| **v0.2**   | Federation-lite scaffolding : `FederationManifest` data layer, `/cluster-info` endpoint on `weft-agent`, manifest signature verify, `weft federation list` (read-only). |
| **v0.3**   | Full primitives : `weft federation join / leave / place`, SRV discovery, cosign-keyless manifest signing, `SchedulingRule.federated=true`. |
| **v0.4+**  | Optional `weft-federation` control plane for operators who want global scheduling. Lite remains the default. |

## What federates, what doesn't

| Federated (intent only) | Per-cluster (physical) |
|---|---|
| Tenant manifests        | VMs, microVMs |
| Network policies        | Volumes, snapshots, backups |
| Image catalogues (refs) | Hosts, scheduling rules |
|                         | Audit logs |

Rule of thumb : identity and policy federate ; physical resources stay
inside the cluster that owns them. Cross-cluster volume replication
and live migration are explicitly **out of scope**.

## Trust

- Federation manifests are **signed** (cosign keyless via GitHub OIDC
  for public-repo deployments, admin signing key otherwise).
  Verification reuses the Sigstore-pinning machinery from
  [`cosign-verify.md`](cosign-verify.md).
- Joining a federation requires **admin on both sides** : peer mints a
  short-lived bootstrap token, joiner runs `weft federation join`
  with it.
- A misbehaving member is evicted by a **quorum of the other
  members** bumping the manifest version.

## What this is not

- It is **not** a global etcd. Each cluster keeps its own state.
- It is **not** a global scheduler in v0.2 ; `weft federation place`
  returns advisory recommendations.
- It is **not** a way to live-migrate VMs across DCs.
- It is **not** required : single-cluster deploys keep working with
  zero federation config.

## Where the work lives

- Design doc : [`docs/design/federation.md`](../design/federation.md).
- Stub package : `federation/` in the `weft` repo — data types,
  JSON roundtrip, signature-verify hook.
- Future code : the `weft federation …` CLI and the `/cluster-info`
  endpoint will land under `cmd/weft/federation/` and `agent/`
  respectively, in v0.2.
