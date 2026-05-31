# Tenant quotas

Weft enforces three per-tenant hard caps at the agent's create
handlers (CreateVM, RegisterMicroVM, CreateVolume). The webui already
displayed quotas ; this document is about the *enforcement* side that
the agent does at the wire layer.

## Today's scoping

At this writing the agent's only tenant-grained registry is the
**project** (per [`weft-uuid-keyed-resources`](../../projects.go)) ;
the `weft_tenant` resource webui ships against does not yet have a
counterpart at the gRPC layer. Quotas therefore key on **project UUID**.
When the tenant model lands, the cap can move up one level without
touching the enforcement call sites — the three `Enforce*` helpers
take a `projectUUID` argument that becomes a `tenantUUID` lookup
once the tenant-to-project mapping exists.

## The three dimensions

| Field | Unit | Sum over | Notes |
|---|---|---|---|
| `cpu_count`   | vCPUs                | project's VMs (`VM.CPUCount`)              | Hard cap on total scheduled vCPUs. |
| `memory_gib`  | GiB                  | project's VMs (`ceil(MemoryMiB / 1024)`)   | Ceil-div so 2049 MiB counts as 3 GiB, not 2. |
| `volume_gib`  | GiB                  | project's volumes (`Volume.SizeGiB`)       | Σ across all volumes in the project. |

A zero value on any dimension means **unlimited** on that dimension.
A fresh project has every dimension at zero, i.e. effectively
unbounded until an operator sets caps.

## Setting a cap

Operators set caps via the `weft.Adapter` API (TODO: CLI surface lands
together with the tenant model — `weft tenant set-quota` reuses the
same helpers). In-process today :

```go
adp.SetTenantQuota(projectUUID, weft.TenantQuota{
    CPUCount:  16,
    MemoryGiB: 64,
    VolumeGiB: 500,
})
```

`SetTenantQuota` persists through the standard registry storage
factory (file backend on single-host dev, etcd on a 3-DC control
plane). Passing a fully-zero `TenantQuota{}` clears the entry —
no separate Delete needed.

## The enforcement contract

When a request would push the project's allocation past one of its
caps, the create handler returns a `codes.ResourceExhausted` gRPC
error with a message that names the dimension :

```
tenant quota exhausted: cpu (allocated 14 + requested 4 > cap 16)
```

CLI + webui clients translate `ResourceExhausted` into the
operator-visible "quota exceeded" toast without needing handler-
specific awareness ; that's the wire contract the helpers commit to.

### Per-handler behaviour

| Handler          | Dimensions checked       | Source of values                          |
|---|---|---|
| `CreateVM`         | cpu_count + memory_gib | `req.Cpu`, `req.MemMb`                    |
| `RegisterMicroVM`  | cpu_count + memory_gib | `(0, 0)` — boot artefacts drive runtime ; the check still trips when the project is already past cap. |
| `CreateVolume`     | volume_gib             | `req.SizeGib`                             |

The `RegisterMicroVM` path passes `(0, 0)` because the request shape
doesn't carry CPU/memory — the boot artefacts (kernel + initrd or
UKI ISO) dictate the runtime shape. We still consult the quota to
catch the case where a project has already gone over budget through
classic VMs and starts spawning microVMs to evade enforcement.

## Implementation

- [`tenant_quotas.go`](../../tenant_quotas.go) — registry +
  `Enforce*` helpers.
- [`cmd/weft/main.go`](../../cmd/weft/main.go) — `CreateVM` +
  `RegisterMicroVM` call sites.
- [`cmd/weft/volumes.go`](../../cmd/weft/volumes.go) — `CreateVolume`
  call site.

Test coverage lives in [`tenant_quotas_test.go`](../../tenant_quotas_test.go) :
round-trip + persistence + per-dimension trip + ResourceExhausted
code assertion + ceil-div memory rounding.

## What this does NOT cover (yet)

- **Tenant-level aggregation** : caps are per-project today. When
  a tenant owns multiple projects, the sum of project allocations
  vs. the tenant cap is enforced by the webui's `Quotas.fits()`
  pre-flight — not the agent. Moving that into the agent waits on
  the `weft_tenant` registry.

- **GPU / network-bandwidth / floating-IP dimensions**. The webui's
  `Quotas` shape already lists them (per `project_webui_huma_typed`) ;
  the agent's `TenantQuota` is a strict subset matching what the
  agent owns. New dimensions extend `TenantQuota` + an `Enforce*`
  helper + a handler hook.

- **Burst / soft caps**. Today's caps are hard — over-budget = deny.
  A future "soft + alarm" mode would deny only above `1.2x cap` and
  fire a `tenant.quota.warned` event between `1.0x` and `1.2x`. Not
  on the roadmap until the tenant model lands.

## References

- [`docs/operations/rbac.md`](rbac.md) — the ACL primitives that
  resolve `projectUUID` before quota enforcement runs.
- Memory `project_webui_huma_typed` — the webui's `Quotas` typed
  shape (the agent's `TenantQuota` is a strict subset).
