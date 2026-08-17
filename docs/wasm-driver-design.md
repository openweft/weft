<!--
SPDX-License-Identifier: BSD-3-Clause
Copyright (c) 2026, openweft contributors
All rights reserved.
-->

# weft-driver-wasm — design note

Status : **proposal, not implemented**. Targets weft-proto **v0.13.0** (new
`WasmWorkloadService`) and a new sibling repo
[`openweft/weft-driver-wasm`](https://github.com/openweft/weft-driver-wasm)
following the same go-plugin pattern as `weft-driver-vz` / `-qemu` / `-vmd`
/ `-dcs` (see [[project_driver_plugins]]).

Audience : operators evaluating whether to expose browser / standalone-WASM
worker pools as compute targets ; contributors implementing the agent +
control-plane glue.

## 1. Motivation

weft today ships three workload kinds :

| Kind | Driver | Isolation | Where it runs |
|---|---|---|---|
| classic VM (`weft instance`) | vz / qemu / vmd / dcs | full hardware virt | dedicated host |
| microVM (`weft microvm`) | qemu (KVM) / vz | KVM + crun + virtio-fs | dedicated host |
| WASM workload (new) | wasm | WASI sandbox | browser tab / standalone runtime |

The 4th kind exists because three real workloads do not need the cost or
the surface area of a Linux microVM :

- **Edge compute.** Analytics aggregation, ML inference (ggml-wasm), CDN-
  style request rewriting. The compute is short-lived, stateless, and
  ships best as a `.wasm` artefact already on the operator's OCI registry.
- **Ephemeral user-contributed compute** (a SETI@home-shaped pattern).
  A browser tab pinned by an institutional user becomes a worker for
  the duration the tab is open ; the workload is fully sandboxed by
  WASI + the host browser, so the operator is comfortable enrolling
  third-party endpoints they do not otherwise trust.
- **Cheap horizontal scaling** for stateless workloads. A microVM costs
  ~50ms cold start + ~80 MiB RAM minimum. A wasmtime instance costs
  ~2ms cold start + ~1 MiB RAM. For functions-as-a-service-shaped
  fan-out we want the cheaper primitive.

The constraint that drives the design : the browser **cannot** host real
Linux VMs (no KVM, no IOMMU, no page-table control). So this is **not**
"another VM driver". It is a new workload kind alongside microVM and
classic VM ; the driver plugs into the same `weft-driver-plugin` gRPC
contract but answers a new `Workload` service rather than the existing
`Hypervisor` service.

## 2. Non-goals (V0.1)

- **Multi-tenant browser worker pools.** V0.1 assumes one worker maps to
  one operator-trusted endpoint (institutional desktop, edge POP). True
  multi-tenant browser-tab enrolment is V0.2+ and needs a separate trust
  story (capability tokens scoped per-tenant + signed module manifests).
- **GPU offload.** WebGPU is the obvious next step but it widens the
  attestation surface significantly. Out for V0.1, revisit in V0.3.
- **Cross-worker scheduling primitives.** No "this workload needs 4
  workers and an MPI mesh between them". One workload = one worker.
- **Stateful persistence on browser workers.** The browser is, by
  definition, ephemeral. Persistence is opt-in via pin-to-edge —
  see § 7.
- **Live migration.** A workload that dies migrates as a fresh start on
  another worker. There is no Wasmtime equivalent of `vz migration`.

## 3. High-level architecture

```
                            +-----------------------+
                            |   weft-agent (host)   |
                            |  WeftAgent gRPC API   |
                            +----------+------------+
                                       |
                              go-plugin (stdio gRPC)
                                       |
                            +----------+------------+
                            |  weft-driver-wasm     |
                            |  (Workload service)   |
                            +----------+------------+
                                       | WebSocket (TLS, mTLS optional)
                            +----------+------------+
                            |       worker          |
                            |  +-----+   +-----+    |
                            |  | wasm|   | wasm|    |
                            |  +-----+   +-----+    |
                            +-----------------------+
                              browser tab            standalone wasmtime
                              (Go→wasm32-js OR        (linux/macos/win,
                               TS WS client)           Go binary embedding
                                                       Wasmtime)
```

Three processes :

1. **weft-agent** — unchanged. Sees a new `WasmWorkloadService` surface
   exactly the same way it sees `WeftAgent.CreateVM` today. The agent's
   Adapter pattern routes Wasm-shaped requests through the driver
   plugin's `Workload` service.

2. **weft-driver-wasm** — go-plugin executable, one per agent host. Owns
   :
   - the **worker registry** (UUID → live WebSocket connection)
   - the **enrolment endpoint** (`POST /enroll`, returns a one-shot
     worker token)
   - the **per-worker WebSocket listener** (workers dial IN — the host
     never has to reach back into a browser tab)
   - **module materialisation** : OCI pull + `application/wasm` mediatype
     unwrap → in-memory `[]byte` ready to ship to the worker
   - **lifecycle dispatch** : translate `CreateWorkload` /
     `StartWorkload` / … into JSON-framed messages over the WebSocket.

3. **worker** — either :
   - **browser-side** : a Go-compiled `wasm32-js` (or a TypeScript
     WebSocket client + dynamic `WebAssembly.instantiate`). The browser
     tab connects out to the agent's public WebSocket endpoint, presents
     the enrolment token, and is added to the pool.
   - **standalone runtime** : a small Go binary that embeds
     [`wasmtime-go`](https://pkg.go.dev/github.com/bytecodealliance/wasmtime-go/v25)
     and exposes the same WebSocket dialler. Runs on operator-managed
     edge POPs, customer-NAT-trapped laptops, jetson-class boards with
     `wasmtime --wasi=preview2 --invoke=...`.

The choice of "worker dials INTO the agent" (instead of the agent dialing
out) is deliberate :
- browsers cannot accept incoming connections,
- NAT/firewall traversal happens at the worker end (typically just `wss://`
  to a public ingress),
- the control plane addresses workers by **uuid** rather than network
  endpoint — same model as [[openweft_pull_model]] for VM agents.

### 3.1 Enrolment flow

```
operator                         worker (browser tab)         weft-agent
   |                                    |                          |
   | weft worker token mint --name ...  |                          |
   |-------------------------------------------------------------->|
   | <----- {token, agent_ws_url, ca_pem} -------------------------|
   |                                    |                          |
   | (out-of-band : QR code / login)    |                          |
   |----------------------------------->|                          |
   |                                    | open https://...?token=  |
   |                                    | wss connect, send token  |
   |                                    |------------------------->|
   |                                    |                          | RegisterWasmWorker
   |                                    | <-- {worker_uuid, ack} --|
   |                                    |                          |
   |                                    | <===== heartbeat (10s) ==>|
```

Tokens are short-TTL (5 min default), bound to the `worker_uuid` minted
by `RegisterWasmWorker`, and the agent stores the worker record in etcd
under `/weft/wasm-workers/<uuid>` so a recovered cluster re-uses the same
identity if the tab reconnects.

## 4. gRPC contract

See `weft-proto/v1/workload.proto` (this PR). Three message families :

- `WasmWorkload` — the operator-facing record. Mirrors `VMInfo` and
  microVM `RegisterMicroVMRequest` field-by-field so the same scheduler
  / planner / quota code consumes it.
- `WasmWorker` — one enrolled browser tab or standalone-runtime endpoint.
  Mirrors `HostInfo` in shape (UUID + agent-relative endpoint +
  properties + state + heartbeat).
- `WasmWorkloadService` — the RPC surface on `WeftAgent`. Standard
  Create / Get / List / Update / Delete + Start / Stop / Restart on
  workloads ; List / Drain on workers. Pattern mirrors v0.9.0 /
  v0.12.0 nouns : UUID-keyed, `update_mask`-style partial PATCH on
  Update via empty-means-keep + `clear_xxx` booleans for collections,
  pagination via `page_token` + `page_size`.

### 4.1 Driver-plugin shape

The new `Workload` gRPC service lives in `weft-driver-plugin/driverpb/`
alongside the existing `Hypervisor` / `Network` / `Volume` / `Image`
services. Methods :

```
service Workload {
  rpc HostInfo(google.protobuf.Empty) returns (HostInfoResponse);
  rpc CreateWorkload(CreateWorkloadRequest)  returns (google.protobuf.Empty);
  rpc StartWorkload (WorkloadUUIDRequest)    returns (google.protobuf.Empty);
  rpc StopWorkload  (WorkloadUUIDRequest)    returns (google.protobuf.Empty);
  rpc DeleteWorkload(WorkloadUUIDRequest)    returns (google.protobuf.Empty);
  rpc ListWorkers   (google.protobuf.Empty)  returns (ListWorkersResponse);
  rpc DrainWorker   (DrainWorkerRequest)     returns (google.protobuf.Empty);
}
```

It carries the same sentinel-error convention as the other services
(`drivers.ErrNotApplicable` / `ErrUnsupported` / `ErrNotFound` round-trip
across the boundary via the `convert.go` helper).

A weft host that does NOT load the wasm driver answers every
`WasmWorkloadService` RPC with `Unimplemented` ; clients fall back to
the microVM path or refuse the operation. Standard
[[project_driver_plugins]] behaviour — drivers are advertised at
`RegisterHost` time via `Drivers[]` so the scheduler never picks a
wasm-less host for a wasm workload.

## 5. Workload lifecycle

Mapped 1-to-1 against the existing gRPC `WeftAgent` lifecycle so the
weft-tui / weft-webui pickers reuse the existing widgets :

| weft RPC | Driver call | Wire effect |
|---|---|---|
| `CreateWasmWorkload` | `Workload.CreateWorkload` | record in etcd ; module pulled via the Image driver ; scheduler picks a worker ; no execution yet |
| `StartWasmWorkload` | `Workload.StartWorkload` | agent ships module bytes + env + capabilities to the worker over its WebSocket ; worker instantiates Wasmtime/V8 instance ; first execution starts |
| `StopWasmWorkload` | `Workload.StopWorkload` | agent sends `stop` frame ; worker calls `engine.IncrementEpoch()` to interrupt ; module is freed |
| `RestartWasmWorkload` | server-side : Stop then Start ; rolls back on Start-half failure (same shape as v0.12.0 `RestartVM`) |
| `DeleteWasmWorkload` | `Workload.DeleteWorkload` | record dropped from etcd ; worker frees module if running |

Idempotent in spirit (same as `DeleteVMOp` today) — a re-delete of a
missing workload surfaces the typed `ErrNotFound`. A double-Start is
a no-op when the workload is already `WASM_STATE_RUNNING`.

### 5.1 Scheduling

The scheduler picks a worker by :
1. Filtering `WasmWorker` records to those in `WASM_STATE_READY` (skips
   `BUSY` if `worker_concurrency` is already saturated, `DRAINING`, and
   `GONE`).
2. Applying the workload's `properties` selector against worker
   `properties` (same selector grammar as
   [[openweft_nominal_binding]] : comma-separated `k=v` pairs).
3. Applying tenant quota gates (per-tenant `wasm_workloads_total`,
   `wasm_memory_total_mib`).
4. Spreading across workers by anti-affinity hint (`anti_affinity` on
   `SchedulingRule` ; we reuse the existing field, scoped to
   `worker` instead of `host`).
5. Picking the lowest-loaded eligible worker (current count of running
   modules / max_concurrency).

Workers that miss N heartbeats (default 3 × 10s) transition to
`WASM_WORKER_GONE` ; every workload pinned to them transitions to
`WASM_STATE_PENDING` and the scheduler re-picks. Implementation lives
under `agent/wasmrespawn/` following the existing
[[project_respawn_v013_true_ha]] watcher pattern.

## 6. Security model

WASM gets its sandbox guarantees from the runtime, not from us. Layered :

### 6.1 Module integrity

- `module_ref` is an **OCI digest-pinned reference** (e.g.
  `ghcr.io/openweft/wasm-hello@sha256:abc…`). The agent's `Image`
  driver pulls + verifies the digest BEFORE the wasm driver sees it.
- Optional [cosign](https://docs.sigstore.dev/) signature verification
  follows the existing supply-chain policy described in
  `docs/operations/supply-chain.md` (cosign verify against an operator-
  pinned key set).
- Workers never pull modules themselves — they receive bytes over the
  authenticated WebSocket. This keeps untrusted code paths out of the
  worker's network stack.

### 6.2 Runtime sandbox

| Worker kind | Runtime | Isolation primitives |
|---|---|---|
| Browser | V8 / SpiderMonkey | tab origin + WebAssembly sandbox + WASI shim is an explicit subset (no `wasi:filesystem` by default) |
| Standalone | Wasmtime | full WASI 0.2 with capability-based ambient authority ; CPU caps via `epoch_deadline` + memory caps via `Config.max_wasm_stack` + `wasmtime::Memory.grow` cap |

The `capabilities` field on `WasmWorkload` enumerates the WASI ambient
authorities the workload may request : `"wasi:http"`, `"wasi:nats"`,
`"wasi:keyvalue@<scope>"`. Empty = pure compute, no I/O ; the worker
links a no-op WASI shim. Capabilities never grant **direct** host
network access ; every `wasi:http` call from the workload is proxied
back through the worker → agent over the WebSocket, so the operator's
egress policy applies. See § 8 for the proxy contract.

### 6.3 Capability tokens

Outbound calls back through the agent carry a per-workload capability
token (short-TTL JWT signed by the agent's `wasm_caps` key). The token
encodes (`workload_uuid`, `tenant_uuid`, `capability`, `scope`).
The agent verifies on every proxied call. Token rotation is automatic
at workload Start.

### 6.4 What the browser can NOT do

- No host filesystem access (no `wasi:filesystem`).
- No raw sockets (`wasi:sockets` is opt-in and proxied).
- No direct DNS resolution (proxied via `wasi:http` only).
- No `wasi:clocks/wall-clock` access beyond second precision (anti-
  timing-oracle).
- No `wasi:random` other than the WebCrypto-backed CSPRNG.

The operator can lock the capability set down further via SchedulingRule
property gates (e.g. `wasm.cap.http=denied` properties on a project's
workload baseline).

## 7. Resource model

Resource accounting follows Wasmtime's deterministic-cap primitives :

- **`memory_mib`** caps the Wasmtime linear memory (and the browser
  module's `WebAssembly.Memory.grow` ceiling).
- **`cpu_fuel`** caps deterministic execution units (Wasmtime `fuel`
  — each instruction consumes ≥ 1 fuel ; module hits the cap → trap →
  `WASM_STATE_FAILED`).
- **`worker_concurrency`** is the per-worker cap on simultaneous module
  instances. Default 1 — one worker runs one module at a time —
  conservative for browser workers whose tab has a single event loop ;
  operators raise it for standalone-runtime workers.

The scheduler enforces `tenant_quotas.wasm_memory_total_mib` and
`tenant_quotas.wasm_fuel_total` at admission, matching the existing
quota surface for VMs.

A standalone-runtime worker can publish a `max_concurrency` higher than
1 ; the browser worker auto-caps at 1 (V0.1).

## 8. Networking

Workloads do not have a virtual NIC. They have a **proxied socket-ish
surface** :

```
+-------------+    wasi-http call     +-------------+   real socket
| wasm module |--------------------->| worker       |---------------+
|             |                       | (WASI shim)  |               |
+-------------+                       +------+-------+               v
                                             |                  +-------+
                                             | WebSocket frame  | weft  |
                                             | {cap_token,      | agent |
                                             |  method, url,    |       |
                                             |  body}           |       |
                                             +----------------->|       |
                                                                |       |
                                                                |+-----+|
                                                                ||proxy||
                                                                |+--+--+|
                                                                +---|---+
                                                                    | real network
                                                                    v
```

The agent then applies the workload's egress policy (allowlist on
hostname / port, per-tenant rate limits) and emits the real call. This
gives the operator one chokepoint to enforce data-residency, audit
outbound DNS, and rate-limit a misbehaving module.

For ingress (the workload acting as an HTTP responder), the agent
exposes a Caddy route ([[project_reverse_proxy_caddy]]) that terminates
TLS publicly and bridges into the worker's WebSocket as an upgraded
stream. The workload sees a `wasi:http/incoming-handler` interface — the
operator-facing public URL is captured under
`WasmWorkload.ingress_url` (server-filled).

Workloads that need to reach into the VPN-side mesh (mesh peers running
inside microVMs) route via the agent too — the agent has the WireGuard
keys ; the worker does not. See [[wireguard_replaces_vxlan]] for why we
do not let the worker join the mesh directly.

## 9. Persistence

**Default : ephemeral.** A WASM workload has no disk. Re-Start on a
different worker is functionally identical to first Start.

**Opt-in : pin-to-edge.** Operators that want a small amount of
sticky state per workload set `pin_host_uuid` on the workload (mirrors
the `host_uuid` field on `StartVMRequest`). The workload is then
restricted to workers that have a [[project_weft_block]] reflink
attachment to the agent's local volume namespace ; the workload's WASI
`keyvalue` capability binds to a per-workload bucket on that volume.
This keeps the persistence cheap (reflink CoW, no replicated block) at
the cost of locality.

For real durable state, the operator's pattern is **WASM workload + S3
bucket** (`BucketInfo` + per-tenant credentials in the secret store, see
the v0.9.0 `BucketInfo` shape). The workload reaches the bucket via
`wasi:http` proxied through the agent ; the S3 bucket has its own
durability story.

V0.1 supports **only the ephemeral path**. Pin-to-edge is V0.2.

## 10. Failure model

| Failure | Detection | Recovery |
|---|---|---|
| Worker tab closed / network drop | WebSocket close + 3 missed heartbeats | worker → `WASM_WORKER_GONE` ; affected workloads → `WASM_STATE_PENDING` ; scheduler re-picks |
| Module trap (fuel exhaustion, OOM, unreachable instruction) | worker `Workload.report_failure` frame | workload → `WASM_STATE_FAILED` ; `RestartPolicy` (mirrors VM `RespawnPolicy`) kicks in |
| Driver-plugin crash | agent's plugin client gets `io.EOF` | agent restarts the plugin process (existing [[project_driver_plugins]] restart logic) ; worker reconnects |
| Agent restart | WebSocket connections drop | workers reconnect with their existing UUID (etcd-persisted) ; in-flight workloads transition to `WASM_STATE_PENDING` |
| Etcd partition | agent's etcd watcher stalls | as for VMs : new operations block until quorum returns ; running workloads keep running (no control-plane round-trip on the data path) |

`StartWasmWorkload` and `StopWasmWorkload` are **idempotent** at the
RPC layer — a duplicate Start on a running workload is a no-op + return
the current `WasmWorkload` ; same for Stop on a stopped workload. This
matches the existing `StartVM` / `StopVM` semantics.

## 11. Where this lives in the catalogue

A new resource appears in the weft-tui `:wasm-workloads` palette view
(see `weft-tui/catalogue.go`) :

```
ID:      "wasm-workloads"
Title:   "WASM Workloads"
Section: "Compute"
```

Columns : Name | State | Worker | Memory (MiB) | Fuel | Created.

A second palette view `:wasm-workers` lists the enrolled worker
endpoints — Name | State | Host (agent) | Modules running / max |
Heartbeat age.

The webui mirrors these under **Compute → WASM Workloads** in the
sidebar (next to Instances + microVMs). No new top-level resource —
the existing VMs primary tab does not absorb wasm workloads because the
mental model is different (no SSH, no console, no NIC).

A `workload.kind` discriminator field is added to `VMInfo` (already
implicit today via the boot mode) for clients that want to render
microVM / instance / wasm in a single flat list :

```
enum WorkloadKind {
  WORKLOAD_KIND_UNSPECIFIED = 0;
  WORKLOAD_KIND_INSTANCE    = 1;  // classic VM
  WORKLOAD_KIND_MICROVM     = 2;
  WORKLOAD_KIND_WASM        = 3;
}
```

…added to `VMInfo` (a future v0.13.0 line item) so the flat-list
clients can reuse a single union view.

## 12. Operator workflow (sketch)

```
# 1. Enrol a worker (mints token + prints QR for the browser tab).
$ weft wasm worker enrol --name edge-paris-01 --concurrency 4
worker_uuid: 1b8c…
token: WTK-eYJh… (valid 5m, single-use)
url:   https://weft.example.com/wasm/connect?token=…

# 2. Browser tab opens the URL → connects → worker appears as READY.
$ weft wasm worker list
UUID   NAME            STATE   MODS   AGE
1b8c…  edge-paris-01   READY   0/4    12s

# 3. Push a wasm workload into the new pool.
$ weft wasm workload create \
    --name analytics-edge \
    --module ghcr.io/acme/analytics-edge@sha256:… \
    --memory 32 --fuel 100000000 \
    --capability wasi:http \
    --property tier=edge

# 4. Watch it run.
$ weft wasm workload list
NAME              STATE     WORKER         MEM   FUEL          CREATED
analytics-edge    RUNNING   edge-paris-01  32    100000000     2s ago

# 5. Drain a worker for maintenance.
$ weft wasm worker drain --uuid 1b8c…
draining ; 3 workloads re-scheduled.
```

CLI lives under `weft wasm {worker,workload} …` with `Aliases:
[]string{"list"}` on every `ls` per [[feedback_cli_ls_alias_list]]. The
tfprovider gets a `weft_wasm_workload` resource following the
[[project_tfprovider_framework]] pattern.

## 13. Observability

- Prometheus : `weft_wasm_workloads{state=…}`,
  `weft_wasm_workers{state=…}`, `weft_wasm_fuel_consumed_total`,
  `weft_wasm_module_pull_seconds`.
- NATS subjects (per [[project_weft_slognats]]) :
  `weft.wasm.workload.<uuid>.{started,stopped,failed,respawn}`,
  `weft.wasm.worker.<uuid>.{enrolled,heartbeat,gone}`.
- Logs : `slog.JSONHandler` fan-out to stderr + NATS, structured with
  `component=wasm-driver`.
- `weft-doctor` picks up `weft.wasm.*` automatically — no change to the
  classifier rules required ([[project_weft_doctor]] is pattern-based,
  not topic-bound).

## 14. Open questions

1. **Worker authentication beyond the enrolment token.** V0.1 trusts
   the bearer token + the TLS pin. Should standalone workers carry an
   mTLS client cert as well? Probably yes for edge POPs ; the QR-flow
   for browser tabs makes mTLS impractical there.
2. **WASI 0.2 vs Component Model.** wasmtime-go's component support is
   stable as of v25 but the JS-side shims are not. Lock V0.1 to core
   modules (preview-1 `wasi:cli`-shaped surface) and ship Component
   Model in V0.2 once the browser story matures.
3. **Module hot-reload.** Today `UpdateWasmWorkload` with a changed
   `module_ref` requires Stop → Start. Could we ship a swap-bytes
   in-place primitive? Adds complexity ; defer to V0.2 unless an
   operator workload demonstrates the latency gap.
4. **Pricing / metering.** Wasmtime fuel maps poorly to dollars (very
   uneven instruction cost). The metering story probably wants
   wall-clock + memory-second pairs. Out of scope for V0.1 ; tracked
   under tenant-quota work for V0.3.
5. **GPU offload (WebGPU).** Browsers expose
   [WebGPU](https://www.w3.org/TR/webgpu/) as the Vulkan-shaped GPU
   surface ; a wasm module compiled with `wasi:webgpu` could run ML
   inference at near-native speed. Out for V0.1, target V0.3 once the
   WebGPU spec is stable across the three engines.
6. **Cross-tenant worker reuse.** A worker physically belongs to one
   trust boundary (the institution that owns the browser tab). Can one
   worker host workloads from multiple tenants? Probably no for V0.1
   (one-tenant-per-worker, gated at enrol) ; revisit when capability
   tokens harden in V0.2.
7. **CRI parity.** Should the wasm driver also satisfy a
   [WasmEdge CRI](https://wasmedge.org/docs/develop/deploy/kubernetes/quickstart)
   surface for cluster operators coming from k8s? Probably yes as a
   late-V0.2 nice-to-have ; the WasmWorkloadService gRPC shape is
   already compatible.

## 15. Migration / compatibility

`weft-proto v0.13.0` is **additive only** :
- new messages (`WasmWorkload`, `WasmWorker`, …)
- new RPCs on `WeftAgent` (10 new : 5 lifecycle + 3 control + 2 worker)
- new `WorkloadKind` enum reused by `VMInfo` (additive — existing
  consumers ignore the field)

No existing field number is reused. Pre-v0.13 clients that don't speak
the wasm surface see `Unimplemented` on every WasmWorkload RPC, which is
the standard fallback per [[project_v06_audit_close]].

`weft-driver-plugin` requires a separate minor bump (driver-plugin
v0.6.0 = + `Workload` service). Hosts that load only the existing four
drivers continue to operate ; the wasm driver is opt-in via
`cluster.hcl` `drivers { wasm = { ref = "ghcr.io/openweft/weft-driver-wasm:v0.1.0" } }`.

## 16. Implementation plan (rough)

| Slice | What lands | Repos touched |
|---|---|---|
| S1 | proto + driver-plugin contract + skeleton plugin | `weft-proto`, `weft-driver-plugin`, `weft-driver-wasm` (new) |
| S2 | agent surface (`WasmWorkloadService` server) + etcd persistence + Adapter wiring | `weft` |
| S3 | standalone wasmtime worker binary + enrolment flow | `weft-driver-wasm`, `weft` (CLI) |
| S4 | browser worker (Go→wasm32-js) + Caddy proxy route | `weft-driver-wasm`, `weft-webui` |
| S5 | scheduler + RespawnPolicy + zombie-gc parity | `weft` |
| S6 | tfprovider resource + CLI parity + webui sidebar entry | `terraform-provider-weft`, `weft-tui`, `weft-webui` |

Estimated 4-6 weeks for S1-S3 (V0.1 = MVP that can run a hello-world
.wasm on a standalone worker enrolled via CLI), another 4 weeks for the
browser side + ingress, another 2 for hardening + observability.

## 17. References

- [[project_driver_plugins]] — go-plugin OCI-pulled driver shape.
- [[project_microvm_first_strategy]] — why microVM stays the default
  path ; wasm is the next-cheaper primitive below microVM, not above.
- [[openweft_pull_model]] — worker-dials-in mirrors the agent-dials-in
  pattern for VM hosts.
- [[openweft_nominal_binding]] — selector grammar reused for worker
  pinning.
- [[wireguard_replaces_vxlan]] — workers do not join the WG mesh ; the
  agent proxies on their behalf.
- [[project_reverse_proxy_caddy]] — ingress route on the workload side
  uses the embedded Caddy.
- [[project_weft_block]] — pin-to-edge persistence (V0.2).
- [Wasmtime fuel docs](https://docs.wasmtime.dev/api/wasmtime/struct.Config.html#method.consume_fuel)
- [WASI Preview 2](https://github.com/WebAssembly/WASI/blob/main/preview2/README.md)
- [OCI mediatype for WASM](https://github.com/solo-io/wasm/blob/master/spec/spec-compat.md)
