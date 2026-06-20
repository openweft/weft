# Webui RPC backlog

Status snapshot of which weft-proto RPCs are surfaced by the dashboard
vs. left for a future session.

Cross-check methodology :

```sh
# Server-side RPCs declared in weft.proto
grep -oE "^\s*rpc\s+[A-Za-z0-9_]+" weft-proto/weft.proto \
  | awk '{print $2}' | sort -u > /tmp/proto.txt

# RPCs the webui's wclient actually calls
grep -rE "rpc\.[A-Z][A-Za-z]+\(" weft-webui/internal/ --include="*.go" \
  | sed -E 's/.*rpc\.([A-Za-z]+)\(.*/\1/' | sort -u > /tmp/used.txt

comm -23 /tmp/proto.txt /tmp/used.txt
```

## ✅ Wired in v0.4.36 → v0.4.41 (June 2026)

| Verb | wclient method | Dashboard surface |
|---|---|---|
| `RestartVM` | `RestartVM` | `POST /api/microvms/{name}/restart` |
| `SetHostCordoned` | `SetHostCordoned` | `POST /api/hosts/{uuid}/cordon` |
| `SetProjectTenant` | `SetProjectTenant` | `POST /api/projects/{name}/tenant` |
| `ResizeVolume` | `ResizeVolume` | `POST /api/volumes/{key}/resize` |
| `GetProjectQuota` | `GetProjectQuota` | enriches existing `GET /api/projects/{name}/quota` |
| `GetTenantQuota` | `GetTenantQuota` | enriches existing `GET /api/tenants/{name}/quota` |
| `SetTenantQuota` | `SetTenantQuota` | existing `PUT /api/tenants/{name}/quota` |
| `SetProjectQuota` | `SetProjectQuota` | existing `PUT /api/projects/{name}/quota` |

## ⚠ Documented as machine-to-machine (not for the dashboard)

These RPCs intentionally have no dashboard surface — they're called
by other weft daemons / agents, not by humans through the UI.

| Verb | Caller |
|---|---|
| `Admit`, `Enroll`, `CompleteEnroll`, `RequestAdmission` | Attestation handshake between weft-agent + weft-control |
| `Connect` | AgentDispatch bidi stream, agent ↔ control |
| `HeartbeatHost`, `RegisterHost`, `RegisterMicroVM` | Cross-agent lifecycle |
| `RenderNATSAuthorization` | Operator CLI only (no dashboard equivalent yet) |

## 📋 Open backlog — wire these next session

Operator actions the server supports today but the dashboard
doesn't expose. Listed by impact :

### High-impact (operator-blocking)

| Verb | Suggested route | Notes |
|---|---|---|
| `DeleteHost` | `DELETE /api/hosts/{uuid}` (already exists as a local-only stub) | Live wire it ; remove the local row deletion path. |
| `DeleteTenant` | `DELETE /api/tenants/{name}` | Cascade behaviour : block when projects exist (see `weft/tenants.go`). |
| `DeleteUser` | `DELETE /api/users/{name}` | Idempotent ; admin-only. |
| `SetHostState` | `POST /api/hosts/{uuid}/state` | Drain / down / active ; pairs with cordon. |
| `SetHostProperties` | `PUT /api/hosts/{uuid}/properties` | Free-form k=v annotations. |
| `SetVMProperties` | `PUT /api/microvms/{name}/properties` | Per-VM annotations. |
| `WatchEvents` | SSE on `/api/events` | Major UX gap. Server already streams ; bridge to `text/event-stream`. |

### Medium-impact (rename / detail flows)

| Verb | Suggested route |
|---|---|
| `RenameNetwork` | `PUT /api/networks/{uuid}` |
| `RenameSecurityGroup` | `PUT /api/security-groups/{uuid}` |
| `GetHost`, `GetAZ`, `GetRack`, `GetUser` | `GET /api/{noun}/{key}` (drawer detail) |
| `WaitVM` | `GET /api/microvms/{name}/wait?state=running` (long-poll) |
| `TriggerZombieSweep`, `GetZombieReport` | Admin tools page |

### Image management (full surface missing)

| Verb | Notes |
|---|---|
| `ListImages` | Mirror `weft image ls` |
| `PullImage`, `PullImages` | Operator-initiated OCI pulls ; needs a long-poll for the manifest+layer fetch |
| `PatchImage` | Rewrite image metadata (architecture stitching) |
| `CleanImages` | GC unused image cache entries |

### Plugin / federation

| Verb | Notes |
|---|---|
| `InstallPlugin` | Dashboard already has the catalogue browser ; wire the install action. |
| `PublishShareToProject` | Cross-project share sharing flow. |

## Mock data scrubbed (June 2026)

Removed in webui commit `277e585` :
- 18 mock seed `Rows` blocks from `internal/server/resources.go`
- Hardcoded image rows in `internal/server/registry.go`
- 3 hardcoded buckets in `internal/server/objectstorage.go`
- 3 hardcoded shares in `internal/server/shares_storage.go`

Result : ~330 lines of fake data gone. The dashboard now renders
empty tables for any resource without a live RPC + no operator
mutation. Resources with `inventory_mock` / `dns_mock` / `security_mock`
CRUD stores still work (operator POST → row appears) but start
empty rather than pre-seeded.

## Service-level gaps (proto declared, server not implemented)

✅ All AgentControlPlane RPCs (`RegisterAgent`, `Heartbeat`,
`AttachDrivers`) implemented in weft v0.4.41 with the agentv1
bindings generated in weft-proto v0.14.0. Driver dispatch still
flows over AgentDispatch ; AttachDrivers exists for the federation
work.

❌ `GuestPodPlane.Attach` (guest.proto) : the bidi vsock stream
between weft-init (PID 1 in the microVM) and weft-agent. Server
not implemented ; the guest currently delivers PodSpec via a
virtio share rather than the stream. Pick up when the runtime
grows live pod telemetry / control needs.
