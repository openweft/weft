# Webui RPC backlog

Closed state snapshot. Every weft-proto RPC the dashboard could
plausibly expose now has a wclient method + a huma endpoint.

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

The diff should leave only the machine-to-machine RPCs (Attestation
+ AgentDispatch + RegisterHost + Heartbeat) — those are intentionally
out of the dashboard's reach.

## ✅ All wired (v0.4.36 → v0.4.42, June 2026)

### Wave 1 (v0.4.36 - v0.4.41)

| Verb | wclient | Dashboard surface |
|---|---|---|
| `RestartVM` | `RestartVM` | `POST /api/microvms/{name}/restart` |
| `SetHostCordoned` | `SetHostCordoned` | `POST /api/hosts/{uuid}/cordon` |
| `SetProjectTenant` | `SetProjectTenant` | `POST /api/projects/{name}/tenant` |
| `ResizeVolume` | `ResizeVolume` | `POST /api/volumes/{key}/resize` |
| `GetProjectQuota` | `GetProjectQuota` | enriches `GET /api/projects/{name}/quota` |
| `Get/Set TenantQuota` | (already wired) | `GET/PUT /api/tenants/{name}/quota` |
| `Get/Set ProjectQuota` | (already wired) | `GET/PUT /api/projects/{name}/quota` |

### Wave 2 (webui [932bc4f](https://github.com/openweft/weft-webui/commit/932bc4f), weft [v0.4.42](https://github.com/openweft/weft/releases/tag/v0.4.42))

| Verb | wclient | Dashboard surface |
|---|---|---|
| `DeleteHost` | `DeleteHost` | `DELETE /api/hosts/{uuid}/live` |
| `SetHostState` | `SetHostState` | `POST /api/hosts/{uuid}/state` |
| `SetHostProperties` | `SetHostProperties` | `PUT /api/hosts/{uuid}/properties` |
| `GetHost` | `GetHost` | `GET /api/hosts/{uuid}/detail` |
| `GetAZ` | `GetAZ` | `GET /api/azs/{uuid}/detail` |
| `GetRack` | `GetRack` | `GET /api/racks/{uuid}/detail` |
| `GetUser` | `GetUser` | `GET /api/users/{uuid}/detail` |
| `DeleteUser` | `DeleteUser` | `DELETE /api/users/{uuid}` |
| `DeleteTenant` | `DeleteTenant` | `DELETE /api/tenants/{uuid}/live` |
| `SetVMProperties` | `SetVMProperties` | `PUT /api/microvms/{name}/properties` |
| `WaitVM` | `WaitVM` | `GET /api/microvms/{name}/wait` (long-poll) |
| `RenameNetwork` | `RenameNetwork` | `PUT /api/networks/{key}` (now live RPC) |
| `RenameSecurityGroup` | `RenameSecurityGroup` | `PUT /api/security-groups/{uuid}/rename` |
| `GetZombieReport` | `GetZombieReport` | `GET /api/admin/zombies` |
| `TriggerZombieSweep` | `TriggerZombieSweep` | `POST /api/admin/zombies/sweep` |
| `ListImages` | `ListImages` | `GET /api/admin/images` |
| `PullImage` | `PullImage` | `POST /api/admin/images/pull` |
| `PullImages` | `PullImages` | (CLI-driven catalogue refresh ; bridge if needed) |
| `CleanImages` | `CleanImages` | `POST /api/admin/images/clean` |
| `PatchImage` | `PatchImage` | (admin-only ; expose when dashboard needs it) |
| `PublishShareToProject` | `PublishShareToProject` | (cross-project share flow ; expose in tenant UI) |
| `WatchEvents` | `WatchEvents` | `GET /api/events/stream` (SSE) |

## ⚠ Documented as machine-to-machine (no dashboard surface)

| Verb | Caller |
|---|---|
| `Admit`, `Enroll`, `CompleteEnroll`, `RequestAdmission` | Attestation handshake (agent ↔ control plane) |
| `Connect` | AgentDispatch bidi stream |
| `HeartbeatHost`, `RegisterHost`, `RegisterMicroVM` | Cross-agent lifecycle |
| `RenderNATSAuthorization` | Operator CLI (`weft admin nats-authz`) |

## Mock data scrubbed (webui commit [277e585](https://github.com/openweft/weft-webui/commit/277e585))

- 18 mock seed `Rows` blocks from `internal/server/resources.go`
- Hardcoded image rows in `internal/server/registry.go`
- 3 hardcoded buckets in `internal/server/objectstorage.go`
- 3 hardcoded shares in `internal/server/shares_storage.go`

~330 lines of fake data gone. The dashboard renders empty tables
for any resource without a live RPC + no operator mutation. Resources
with `inventory_mock` / `dns_mock` / `security_mock` CRUD stores keep
their helpers but start empty.

## Service-level gaps : all closed

| Service | State |
|---|---|
| `WeftAgent` (weft.proto) | ✅ all 170+ RPCs implemented |
| `AttestationService` | ✅ all 3 RPCs |
| `AgentDispatch.Connect` | ✅ wired (production driver path) |
| `Introspect.ListProcesses` | ✅ wired (weft-microvm-agent) |
| `AgentControlPlane` (agentv1) | ✅ wired (weft v0.4.41 ; weft-proto v0.14.0 Go bindings) |
| `GuestPodPlane` (guestv1) | ✅ wired (weft v0.4.42 ; bidi stream handler + Hello/Ack/drain protocol) |

Every gRPC service declared in weft-proto now has a server-side
implementation.

## AF_VSOCK transport binding (v0.4.43)

Closes the last GuestPodPlane follow-up. The agent's gRPC server
now optionally binds an AF_VSOCK listener for the guest transport :

```sh
weft agent --vsock-port=7777
# or in cluster.hcl :
#   weft { vsock_port = 7777 }
```

Guests dial `VMADDR_CID_HOST=2` + the configured port. Every gRPC
service the agent registers is reachable over vsock — GuestPodPlane
+ WeftAgent + AgentControlPlane all on the same listener.

Linux-only by design (AF_VSOCK is a Linux address family). On
darwin/freebsd hosts the agent logs that vsock was requested but
unavailable + falls back to Unix/TCP/SSH listeners.

Live-validated on the 6-host 3-DC cluster : all 6 agents log
"AF_VSOCK gRPC listening on port=7777".
