# RBAC model

Weft's access-control model is **group-based, scope-aware, verb-typed**.
A caller's identity comes from OIDC claims (`groups`, `email`, `sub`) ;
the gRPC interceptor in `auth.go` builds a `*Caller` and parks it on
the request context. ACL checks downstream look at the caller's
groups + the resource's scope + the operation's verb.

Today's wire-level checks live in [`acl.go`](../../acl.go). This document
captures the model they encode, so future handlers add their own checks
without re-deriving the convention.

## The four pieces

```
┌──────────┐       ┌──────────┐       ┌────────┐       ┌─────────┐
│ Subject  │  ───▶ │ Verb     │  ───▶ │ Object │ in ──▶│ Scope   │
└──────────┘       └──────────┘       └────────┘       └─────────┘
```

| Piece | Today (in code) | Examples |
|---|---|---|
| **Subject** | `*Caller` (OIDC-derived) | `alice@example.com` with groups `[weft:admin, weft:project:abc]` |
| **Verb** | RPC name + implicit op | `CreateVM`, `DeleteVolume`, `ListNetworks`, `Read` |
| **Object** | Typed resource | `vm`, `volume`, `network`, `security_group`, `tenant`, `host`, `scheduling_rule`, `image`, `keypair`, `endpoint`, `deployment` |
| **Scope** | The container the object lives in | `cluster` (everything), `project:<uuid>` (per-project) |

The **subject ↔ scope** edge is mediated by OIDC groups: a caller in
group `weft:project:abc` has project-scoped permissions on project `abc` ;
a caller in `weft:admin` has cluster-scoped permissions everywhere.

## Group naming convention

| Group | Means |
|---|---|
| `weft:admin` | Full cluster access. Bypasses every project-scope check. |
| `weft:project:<uuid>` | Owner of the named project. Full CRUD on resources inside it. |
| `weft:project:<uuid>:viewer` | Read-only access to the named project (proposed; not yet enforced). |
| `weft:tenant:<uuid>` | Full access to the named tenant (proposed for tenant-level RBAC). |

The prefix `weft:` is fixed — operators can't issue groups outside this
namespace through Dex without explicit allow-listing.

## Permission table

|             | cluster ops | project ops | other tenants' ops |
|---|---|---|---|
| `weft:admin`                    | ✓        | ✓ (any)      | ✓        |
| `weft:project:<uuid>`           | ✗        | ✓ on `<uuid>`| ✗        |
| `weft:project:<uuid>:viewer` ¹   | ✗        | read-only on `<uuid>` | ✗ |
| anonymous (no OIDC)             | ✗        | ✗            | ✗        |
| dev caller (`--oidc-issuer=""`) | ✓        | ✓            | ✓        |

¹ Proposed but not yet enforced ; today, group membership grants full
project access regardless of suffix. The `:viewer` parsing is a planned
extension.

## How a handler checks

Every gRPC handler that touches a resource calls one of three primitives,
listed in increasing strictness:

1. **`acl.RequireAdmin(ctx, op)`** — fail unless the caller is in
   `weft:admin`. Cluster-scoped ops only (e.g., `RegisterHost`,
   `DeleteHost`, scheduling-rule changes).

2. **`adapter.AuthorizeProject(ctx, nameOrUUID)`** — fail unless the
   caller owns the named project (group `weft:project:<uuid>`) OR is
   admin. Returns the resolved project UUID so the handler can index
   into the registry under it.

3. **`adapter.VisibleProjects(ctx)`** — returns the set of project
   UUIDs the caller can see (for List operations that filter to
   own-project-only).

The dev caller (when `--oidc-issuer=""`) implicitly passes all three —
that's the single-host dev mode.

## Resource ↔ scope matrix

This is the source-of-truth for which scope each resource type lives in.
Handlers MUST consult this when deciding which check to call.

| Resource type                  | Scope            | Primary check                    |
|---|---|---|
| **Cluster-scoped (admin only)**                                                       |
| `host` (RegisterHost/DeleteHost/…) | cluster        | `RequireAdmin`                 |
| `network`                      | cluster          | `RequireAdmin`                   |
| `scheduling_rule` (proposed)   | cluster          | `RequireAdmin`                   |
| `tenant` (proposed)            | cluster          | `RequireAdmin`                   |
| **Project-scoped (caller owns the project)**                                          |
| `vm` / `instance`              | project          | `AuthorizeProject`               |
| `volume`                       | project          | `AuthorizeProject`               |
| `volume_snapshot`              | project          | `AuthorizeProject`               |
| `security_group`               | project          | `AuthorizeProject`               |
| `image`                        | project          | `AuthorizeProject`               |
| `image_patch`                  | project          | `AuthorizeProject`               |
| `images` (bulk)                | project          | `AuthorizeProject`               |
| `keypair`                      | project          | `AuthorizeProject`               |
| `endpoint`                     | project          | `AuthorizeProject`               |
| `deployment`                   | project          | `AuthorizeProject`               |
| **Filtered List**                                                                     |
| any `List*` RPC                | project + caller | `VisibleProjects` then filter    |

## Verb mapping

Most Weft RPCs map cleanly to the standard CRUD verbs ; a few don't and
get special treatment.

| Verb pattern   | Examples                          | Check |
|---|---|---|
| `Create*` / `Register*`        | CreateVM, RegisterHost, CreateVolume   | scope-appropriate |
| `Get*` / `List*` (read)        | GetVM, ListVolumes                     | scope-appropriate, filter for List |
| `Update*` / `Rename*` / `Resize*` | RenameVolume, ResizeVolume          | scope-appropriate |
| `Delete*` / `Deprovision*`     | DeleteHost, DeprovisionVM              | scope-appropriate |
| **Imperative (not CRUD)**                                                          |
| `StartVM` / `StopVM` / `WaitVM` | VM lifecycle actions                  | `AuthorizeProject` on the VM's project |
| `CleanImages`                  | imperative janitor                     | `RequireAdmin` |
| `WatchEvents` (stream)         | server-streamed events                 | filtered server-side via `VisibleProjects` |

## What the proxy plane needs

The reverse-proxy data plane (Caddy + ACME) is independent of weft
RBAC — once a Route is in etcd (`/weft/proxy/routes/<host>`), Caddy
serves it. The RBAC check applies to **who can write the route table**,
not who can hit the resulting URLs. Today that's the etcd write path,
which is project-scoped via the future `weft_route` resource.

## What this model does NOT cover (yet)

- **Sub-project roles** (viewer vs editor vs admin within one project).
  Today, project group membership = full access. The `:viewer` suffix
  in the group naming convention is reserved but not enforced.

- **Per-resource ACL** (e.g., "alice can read this VM but not these
  other VMs in the same project"). Not on the roadmap — too fine-grained
  for the operator audience we serve.

- **Audit log** of RBAC decisions. Today every check is silent ; failed
  checks emit a `permission denied` error but nothing logs the
  caller/verb/object/decision tuple for post-hoc review. Audit log =
  follow-up, blocks SOC 2 etc.

- **Tenant-scoped roles**. The `weft_tenant` resource exists ; group
  naming `weft:tenant:<uuid>` is reserved ; but handler-side enforcement
  isn't wired. Lands together with the `:viewer` extension above.

## How to add a check to a new handler

1. Determine the resource's scope (consult the matrix above).
2. Call the appropriate primitive at the top of the handler:

   ```go
   func (s *server) CreateVolume(ctx context.Context, req *pb.CreateVolumeRequest) (*pb.CreateVolumeResponse, error) {
       projectUUID, err := s.adapter.AuthorizeProject(ctx, req.GetProject())
       if err != nil {
           return nil, err  // returns gRPC PermissionDenied
       }
       // … rest of the handler, indexing under projectUUID
   }
   ```

3. For `List*` handlers, fetch `VisibleProjects` once + filter
   server-side. NEVER trust a `project` field in the request without
   re-checking it against the caller's visible set.

4. Add a test that asserts the check trips for a caller missing the
   group. Pattern in `acl_test.go` — set up two callers, run the
   handler with each, expect `codes.PermissionDenied` from the second.

## Smoke test

The OIDC end-to-end smoke (in `weft-webui`) covers the auth side — it
proves Dex tokens reach the agent and produce a `*Caller`. The RBAC
side has no equivalent smoke today ; it's exercised through unit tests
per handler (`acl_test.go`, `weft_tenant_acl_test.go`, …). Adding a
"deny + allow" pair to every new ACL-relevant unit test is the
discipline ; an integration drill is a follow-up.

## References

- [`acl.go`](../../acl.go) — current implementation of all three primitives.
- [`auth.go`](../../auth.go) — OIDC validation, `Caller` shape, interceptors.
- Memory `openweft_nominal_binding` — how project-binding is encoded on
  scheduling rules (touches the visible-set logic for List).
- Memory `feedback_no_autopublish_dev` — adjacent operational convention.
