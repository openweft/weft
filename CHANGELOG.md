# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.21] - 2026-06-14

### Changed

- **`SearchRegistryRemote` : implementation upgrades from server-side
  stub (returns name only) to a real `/v2/_catalog` dialer** with
  optional Bearer auth refresh, pagination via `Link` header, and
  `InsecureSkipVerify` gating on the RegistryRemote's `insecure` flag.
  New pure-Go package `registryclient/` (CGO=0) hosts the dialer ;
  the handler in `cmd/weft/main.go` instantiates one `CatalogClient`
  per call, applies a 10s per-request + 30s total timeout, and
  surfaces upstream HTTP status verbatim on auth-gated catalogues
  (ghcr.io private repos, Harbor with admin-only catalog).
  Authenticated catalogue access via `credential_secret_ref` is
  intentionally deferred ; public catalogues (Docker Hub, ghcr.io
  public namespaces, public Harbor) work today.

### Added

- **Server : tenant registry RPCs implemented** — `ListTenants`,
  `CreateTenant`, `DeleteTenant`, `AddTenantAdmin`, `RemoveTenantAdmin`,
  `AddTenantMember`, `RemoveTenantMember` were declared in
  weft-proto v0.6+ and the `weft tenant ...` CLI already called them,
  but no server-side implementation existed — every call returned
  `rpc error: code = Unimplemented`. This commit lands the missing
  pieces : `tenants.go` (JSON-backed registry mirroring `azs.go`),
  `Adapter` helpers + `VZAdapter` interface extension, `initTenants`
  wired into `NewWithStorage`, and seven gRPC handlers in
  `cmd/weft/main.go` (all `RequireAdmin` — tenant management is a
  platform-admin op). `DeleteTenant` is unconditional today because
  the `Project` struct does not carry a `TenantUUID` field yet ;
  cascade-refuse-on-blocking-count is plumbed through the registry's
  `delete()` signature so the slot is ready for the linkage migration.
  Live cluster test on the 3-DC bring-up surfaced the gap.

- **Tier 4-6 parity wave : 6 resource families + 22 RPCs + 5 new CLI
  packages** — closes the last CLI-vs-webui parity gap surfaced by
  the audit. Bumps weft-proto consume to v0.9.0 ; server-side
  registries + handlers + CLI all land in one commit.

  - **VolumeProperty (3 RPCs)** — mirror of VMProperty addressed by
    `volume_uuid`. Server : `volumeproperties.go` + adapter wrapper.
    CLI : `weft volume property {set, get, delete}` (nested under
    `weft volume`).
  - **Share extensions (3 RPCs)** — closes the v0.8 gap : server
    registry shipped (`shares.go` + `CreateShare` / `DeleteShare`
    handlers, previously Unimplemented), plus `GetShare` +
    `ResizeShare` added. CLI : new `weft share resize` + show
    switched to `GetShare` RPC.
  - **Bucket (6 RPCs)** — S3 bucket catalogue (data on versitygw /
    CubeFS objectnode). Server : `buckets.go` + handlers. CLI :
    `weft bucket {ls, show, create, rm, policy {get, set}}`.
  - **SSH-key catalogue (4 RPCs)** — cluster-wide named keys
    distinct from per-VM `weft instance sshkey`. Server :
    `sshkeycatalogue.go` (SHA256 fingerprint + idempotent on
    fingerprint). CLI : `weft sshkey-catalogue {ls, add, rm, import}`.
  - **Scheduling rule (4 RPCs)** — per [[openweft_nominal_binding]],
    nominal placement rules with selector + target_count +
    anti_affinity. Server : `schedulingrules.go`. CLI :
    `weft scheduling-rule {ls, create, update, rm}` (alias `sr`).
  - **Registry remote (4 RPCs)** — OCI registry alias catalogue.
    Server : `registryremotes.go` (upsert on name, partial PATCH
    on endpoint). CLI : `weft registry {ls, set, rm, search}`.
    `SearchRegistryRemote` server-side : stub returning the
    registry name (upstream catalogue dialer is follow-up).

  - **Adapter glue** : new fields on `Adapter` for the six
    registries, `initResources()` wired into `NewWithStorage`,
    `VZAdapter` interface methods extended.
  - **testutil** : Fn surface + handler stubs added for every new
    RPC so CLI tests mock the wire end-to-end.

- **CLI : `weft share` CRUD verbs + `weft instance sshkey import`** —
  Tier-4 parity follow-up. The `share` group grew `ls`, `show`,
  `create`, `rm` on top of the existing `attach`/`detach` ; each new
  verb threads the proto-declared `ListShares` / `CreateShare` /
  `DeleteShare` RPCs. `instance sshkey import` reads an
  `authorized_keys`-style file (one OpenSSH line per line, `#`
  comments + blank lines skipped) and `AddVMSSHKey`s each entry —
  the server's idempotent-on-fingerprint contract makes re-import a
  safe no-op for known keys.
  - **testutil** : stubs added for `ListShares`, `CreateShare`,
    `DeleteShare`, `PublishShareToProject` so the new tests can mock
    the wire without standing up a real server.
  - **gap** : `Share` registry handlers are still server-side
    Unimplemented (proto declares them since v0.8.0, no server
    glue). The CLI lands now so it has a stable target ; runtime
    `Unimplemented` until the server handlers ship.

### Tier 4-6 parity gap — deferred to weft-proto v0.9.0

Reconnaissance for the Tier 4-6 audit (Volume metadata/property,
Share NFS resize/show, Bucket S3, SSH-key catalogue, Scheduling-rule,
Plugin enable/disable, Registry remote) found that most of these need
both a proto bump AND server-side registries. Per-VM property /
sshkey / UEFI are already wired under `weft instance {property,
sshkey, uefi}` and don't need a sibling top-level. Remaining gaps,
each requiring weft-proto v0.9.0 :

- **Volume metadata/property** — distinct from VM property ; needs
  `ListVolumeProperties` / `SetVolumeProperty` / `DeleteVolumeProperty`.
- **Share NFS resize/show** — `ResizeShare` + `GetShare` RPCs (proto
  already has `ListShares` / `CreateShare` / `DeleteShare` ; the
  catalog verbs landed in this drop).
- **Bucket S3** — 7 RPCs (`ListBuckets`, `CreateBucket`,
  `DeleteBucket`, `GetBucket`, `SetBucketPolicy`, `GetBucketPolicy`,
  `ListBucketObjects`).
- **SSH-key catalogue** — cluster-wide named key catalog (different
  from per-VM `weft instance sshkey`). Needs
  `{List,Set,Delete,Import}SSHKey`.
- **Scheduling-rule** — `{Create,Update,Delete,List}SchedulingRule`
  (not the host-cordon flag, which is a separate concern). Today
  `SchedulingRule` only exists as a label on VM placement.
- **Plugin enable/disable** — semantic alias for
  `InstallPlugin` / uninstall ; can land without a proto bump as a
  CLI alias once the verbs are finalised.
- **Registry remote** — `{List,Set,Delete,Search}RegistryRemote` for
  pull-secret + mirror configuration.

Total : ~22 new RPCs across 5 resources. Held back from this drop
because the surface is large enough to warrant its own proto bump
window with cross-repo coordination (server registries, etcd
schemas, webui surfaces).

- **CLI : `weft subnet` + `weft loadbalancer` + `weft dns-zone` +
  `weft dns-record`** — closes the last 17 verbs of the Tier-3
  CLI parity gap on top of weft-proto v0.8.0. Four new noun
  groups, ~19 verbs, cascade-aware delete error surfaces.
  - `weft subnet {ls,show,create,update,rm}` — per-network IP
    scopes. `update` accepts `--clear-dns` to drop the inherited
    list (the proto's `clear_dns_servers` bool disambiguates
    "keep" from "clear" at the wire).
  - `weft loadbalancer {ls,show,create,update,set-backends,rm}`
    (alias `lb`) — project-scoped VIPs. `--backends` parses
    `addr@weight,addr@weight,...` strings ; `set-backends`
    replaces the pool atomically (`SetLoadBalancerBackends`).
    Delete surfaces `blocked_by_fips` when a FIP still maps.
  - `weft dns-zone {ls,show,create,update,rm}` — per-project
    authoritative apex. `show` accepts either UUID or FQDN.
    Delete surfaces `blocked_by_records` so the operator drains
    in one pass.
  - `weft dns-record {ls,create,update,rm}` — zone children. TTL
    and Priority both use `-1` as the "keep current" sentinel
    on update (proto3 int32 has no nil).

  Each package mirrors `weft az` / `weft rack` for table +
  `--format=json` rendering.

- **Server : Subnet + LoadBalancer + DNSZone + DNSRecord registries**
  — 4 new files (`subnets.go`, `loadbalancers.go`, `dnszones.go`,
  `dnsrecords.go`) + `network_plane_adapter.go` for the cascade
  helpers. JSON-document persistence via the Storage interface,
  same pattern as `azs.go` / `racks.go`. Wired through
  `initNetworkPlane` in `Adapter.NewWithStorage`. Adapter
  publishes `subnet.*` / `loadbalancer.*` / `dnszone.*` /
  `dnsrecord.*` events on every mutation.

- **gRPC handlers** for the 20 new RPCs in `cmd/weft/main.go`.
  All write paths gated by `RequireAdmin` ; read paths open to
  any authenticated caller. `resolveProjectUUID` helper shared
  across LB + DNS handlers so the wire `project` field accepts
  either UUID or display name.

- **proto v0.8.0 consumed** : `go.mod` bumped from `v0.7.0` ;
  vendor refreshed via `GOWORK=off go mod tidy && go mod vendor`.

- **CLI : `weft floating-ip` package** — Tier 3 of the webui-vs-CLI
  parity audit. Wires the five FloatingIP RPCs that already shipped
  in the proto (no proto bump) into a dedicated cobra group, alias
  `fip`.
  - `weft floating-ip ls [--project=<name|uuid>] [--format=json]` →
    `ListFloatingIPs`.
  - `weft floating-ip show <uuid|address>` — client-side filter on
    `ListFloatingIPs` (the proto has no `GetFloatingIP` RPC ; fine
    for an operator CLI, the webui keeps its scoped query path).
  - `weft floating-ip allocate --network=<name|uuid> [--project=<...>]`
    → `AllocateFloatingIP`.
  - `weft floating-ip release <uuid>` → `ReleaseFloatingIP`.
  - `weft floating-ip map <uuid> --target=<name> [--kind=vm|lb]`
    → `MapFloatingIP`. `--kind` defaults to `vm`.
  - `weft floating-ip unmap <uuid>` → `UnmapFloatingIP`.

  **Skipped (no RPC in v0.7.0, future proto bump)** :
  - `weft subnet {create, update, delete, ls}` — no
    `{Create,Update,Delete,List}Subnet` RPCs. Subnets currently
    live as fields of `NetworkInfo` ; promoting them to first-class
    requires a `Subnet` message + four RPCs.
  - `weft loadbalancer {create, delete, set-backends, ls, show}` —
    no `{Create,Delete,SetBackends,List,Get}LoadBalancer` RPCs.
    The data-plane note in `MapFloatingIP` references LB targets,
    but the LB registry itself is not yet on the wire.
  - `weft dns-zone {create, update, delete, ls}` — no
    `{Create,Update,Delete,List}DNSZone` RPCs. Only
    `SetNetworkDNS` exists today (resolver config on a network ;
    not authoritative-zone management).
  - `weft dns-record {create, update, delete, ls}` — no
    `{Create,Update,Delete,List}DNSRecord` RPCs ; same gap as
    dns-zone.

- **Inventory hierarchy elevated to the control plane (weft-proto v0.7.0)**.
  AZs and racks are no longer webui-local persistence — they live in
  the same UUID-keyed registry shape as projects + hosts, with their
  own RPCs and JSON-backed storage under the Adapter.
  - `Adapter.AZs / AZByUUID / AZByCode / AZRackCount / AZHostCount /
    CreateAZ / UpdateAZ / DeleteAZ` — DeleteAZ refuses while racks
    or hosts still reference the AZ and surfaces the blocking
    counts on the response.
  - `Adapter.Racks / RackByUUID / RackHostCount / CreateRack /
    UpdateRack / DeleteRack` — CreateRack rejects an unknown
    `az_uuid` parent ; (az_uuid, code) is the secondary uniqueness
    key (the same rack code can repeat across AZs).
  - 10 gRPC handlers under the WeftAgent service : `ListAZs`,
    `GetAZ`, `CreateAZ`, `UpdateAZ`, `DeleteAZ`, `ListRacks`,
    `GetRack`, `CreateRack`, `UpdateRack`, `DeleteRack`. Read RPCs
    are open ; mutations are `RequireAdmin`. `toAZInfo` /
    `toRackInfo` fill the derived counts so clients never need a
    second round-trip.
  - 21 unit tests on the registries (round-trip persistence,
    idempotent create, partial PATCH on update, cascade safety on
    delete, RFC-4122 v4 UUID shape).
- **CLI : `weft az` + `weft rack` packages** — pre-existing webui
  inventory surface now reachable from the operator's shell.
  - `weft az ls` / `show <code|uuid>` / `create <code> [--name --region --status]` /
    `update <code|uuid> [--name --region --status]` / `rm <code|uuid>`
    (cascade error surfaces the blocked count).
  - `weft rack ls [--az=<code|uuid>]` / `show <uuid>` /
    `create <code> --az=<...> [--name --status --height-u]` /
    `update <uuid> [--name --status --height-u]` / `rm <uuid>`.
    `--height-u` is a partial-PATCH sentinel : default `-1` keeps
    the current value, explicit `0` clears it, any positive
    value sets it.
  - `az.ResolveArg` is exported so `weft rack create --az=DC-A`
    accepts either a code or a UUID for the parent. JSON output
    via `--format=json` mirrors the other registry verbs.

  Closes the Tier 1 gap surfaced by the CLI parity audit ; the
  webui's local inventory store stays in place for now (Phase E
  of the proto bump will migrate it to the live RPC).

- **CLI : `weft tenant` package** — full tenant-registry surface so
  a fresh cluster can be brought up through SSH-only access without
  the webui : `weft tenant ls` / `create <name> [--domain]` /
  `rm <name|uuid>` / `add-admin <tenant> <email>` /
  `remove-admin` / `add-member <tenant> <email> [--group ...]` /
  `remove-member`. Name-or-UUID resolution mirrors `weft project`.
- **CLI : `weft quota` package** — read + patch tenant + project
  quotas : `weft quota tenant get <name|uuid>` /
  `weft quota tenant set <t> --vcpu=N --ram-gib=N ...` /
  `weft quota project get` / `weft quota project set`.
  Eleven dimensions (vcpu, ram-gib, volumes, volumes-gib, shares,
  shares-gib, buckets, buckets-gib, registry-gib, floating-ips,
  projects) drawn from a single `quotaDims` table so the flag set
  and the table output stay in lockstep. `set` is a partial PATCH —
  unset flags reuse the live value (explicit `--vcpu=0` is honoured
  as "disable this dimension"). Read-modify-write protects against
  accidentally zero-ing other dimensions ; server still rejects any
  delta that would shrink the cap below `allocated`.

  Both packages cover the Tier 2 (multi-tenant) gap surfaced by the
  CLI-vs-webui parity audit. Tier 1 (az/rack CRUD) stays open : the
  webui's inventory model is webui-local persistence, not exposed
  via gRPC ; elevating it would need a proto bump. Host-side CRUD
  is already complete (`weft host ls/show/register/set-state/set-labels/rm`).

- **Catalogue : `irods-ha`** — three iRODS catalog providers (BSD-3-Clause)
  on a shared `postgres-ha` catalog, one per DC. Managed by the new
  `weft-ha-irods` Go agent (zone bootstrap with etcd advisory lock,
  zone-key minting + seeding, role API at `:8009` for the L4 Caddy
  active probe). Kind = `data-management`.
- **Catalogue : `forgejo-ha`** — three Forgejo Git-forge replicas
  (AGPLv3+ upstream, packaged unmodified ; the agent is BSD-3) on
  shared Postgres + S3 (versitygw-ha or external), one per DC.
  Managed by the new `weft-ha-forgejo` Go agent (install bootstrap
  with shared-secret minting + seeding so `SECRET_KEY` /
  `INTERNAL_TOKEN` / `LFS_JWT_SECRET` agree across replicas, role
  API at `:3001`). Kind = `git-forge` ; distinct from
  `forgejo-runners-ha` (CI workers).
- **Federation : `AdminKeyVerifier`** — multi-key (ed25519 + RSA ≥2048)
  PEM-bundle verifier with implicit sig-size dispatch (64 bytes →
  ed25519 keys, larger → RSA-PKCS1v15-SHA256). Supports rotation
  (verify accepts any enrolled key), `SignRSA` companion. Replaces
  the `DenyAllVerifier`-only path for air-gapped / SSH-key-rooted
  federations that don't have a public OIDC issuer ; cosign-keyless
  stays the next addition.
- **Catalogue : `versitygw-ha`** — three-replica versitygw S3 gateway
  (Apache-2.0) over weft-block-replicated volumes. Replaces the
  removed `minio-ha` plugin (AGPL policy violation per
  `feedback_no_minio`).
- **Catalogue : `postgres-ha` v2** — switched from upstream Patroni
  to our native `weft-ha-postgresql` operator. New image
  `ghcr.io/openweft/postgres-ha:v0.2.0` bundles Postgres + the Go
  agent ; etcd DCS + VMFencer (hard-stops a fenced primary via
  weft-agent `StopVM` before promoting) + Caddy active-health-check
  routing at `:8008/primary`. Structural advantage over Patroni :
  we own the substrate, so we PROVE the old primary is dead rather
  than trust a guest-side watchdog.
- **Inventory : `Volume.Backend` propagated end-to-end** through
  `weft-proto` → `weft-agent` → `wclient` → webui. Backend-aware
  affordance gating on the dashboard's snapshot Revert + Backup
  actions (block-only).
- **WebUI : Groups before Users in the Identity sidebar** + new
  `GroupsTreePage` collapsible tree (groups by tenant).
- **Inventory U-occupancy** — `Rack.HeightU` + `Host.PositionU` +
  `Host.HeightU` ; validation refuses overlaps in the same rack ;
  2D rack-elevation viz draws hosts at their absolute U slot with
  height = chassis size ; 3D iso view scales rack height by total U.

### Changed

- **Per-project ACLs : project-scoped mutations now gate on
  `AuthorizeProject(project_uuid)` instead of `RequireAdmin`
  global.** Any project member (dex `project:<uuid>` group, weft
  `Project.Members` entry, or platform-admin / dev mode) can now
  Create / Update / Delete the resources their project owns ;
  before, only platform admins could because the Tier 1-6 RPCs
  landed before the ACL layer was wired. Affected handlers in
  `cmd/weft/main.go` : Subnet (create/update/delete), LoadBalancer
  (create/update/set-backends/delete), DNSZone (create/update/delete),
  DNSRecord (create/update/delete), VolumeProperty (set/delete),
  Share (create/resize/delete), Bucket (create/delete/set-policy),
  VMProperty (set/delete), UEFIVar (set/delete), VMSSHKey
  (add/remove) — 26 mutations in total. By-UUID handlers resolve the
  owning project via the parent (Subnet → Network → Project ;
  DNSRecord → DNSZone → Project ; VolumeProperty → Volume →
  Project) through new `authSubnet` / `authLoadBalancer` /
  `authDNSZone` / `authDNSRecord` / `authShare` / `authBucket`
  helpers, mirroring the existing `authNetwork` / `authVolume`
  pattern in `networks.go` / `volumes.go`. Cluster-wide resources
  (AZ, Rack, Host, RegistryRemote, SSHKeyCatalogue, SchedulingRule,
  Flavor, Script, Project create/rename/delete, ProjectMember
  add/remove) stay `RequireAdmin` — they touch state outside any
  one project's boundary.

- **`AttachDriverSet` / `AttachDrivers` over the gRPC control plane
  now accept the call and return `nil`** instead of
  `ErrAttachSetUnsupported`. Handles are local in-process pointers ;
  the wire-side per-kind capability list already travels via
  `RegisterHost.Drivers`, and dispatch flows over the
  `AgentDispatch.Connect` bidi stream. The previous Unsupported
  scaffold forced multi-plugin Apple-Silicon agents to fall back to
  single-handle dispatch when the CP was remote — fixed.

### Removed

- **`minio-ha` plugin from the catalogue + the `minio-ha` row from
  `docs/catalogue/`** (AGPL policy violation, replaced by
  `versitygw-ha`). All cross-refs scrubbed across docs +
  `pluginstore/install_test.go` + manifest comments.

- **Volume snapshot/backup dispatch + RPC handlers**.
  - `Volume.Backend` field (`file` default, `block` for weft-block) ; HCL round-trip is backwards-compatible (emitted only when non-default).
  - `Adapter.RegisterVolumeSnapshot` + `DeleteVolumeSnapshotByUUID` dispatch on `parent.Backend` : `file` → existing reflink CoW clone ; `block` → driver-side controller snapshot via `weft-block` (over the `drivers.VolumeDriver` interface).
  - New `Adapter.RevertVolumeSnapshotByUUID` (block-only ; file-backend parents reject with a clear error).
  - New `CreateVolumeBackup` / `ListVolumeBackups` / `DeleteVolumeBackup` / `RestoreVolumeBackup` on `Adapter` + matching gRPC handlers. Restore discovers backup size from sidecar metadata (no proto surface change needed). Targets : `oci://` (recommended), `s3://` (versitygw / CubeFS objectnode), `sftp://` (sftpgo), `fs:///` (dev).
  - CLI subcommands `weft volume snapshot revert` + `weft volume backup create/list/delete/restore`.
  - `Adapter.VolumeOn` reaches the block driver via a `Name() == "block"` walk over the dispatch table — any host running weft-block can service any block volume.
- **Share fan-out widening**. `AttachShareToProject` + `DetachShareFromProject` lifted into the `VZAdapter` interface ; the defensive type assertion + `codes.Unimplemented` in `PublishShareToProject` are gone (mock adapters that forget to wire share now fail at compile time).

- **SecurityGroup → nftables enforcement (full pipeline)**. The proto's
  SecurityGroup RPCs were already implemented at the registry level,
  but the **data plane was missing** — rules were stored, nothing
  filtered packets. New `firewallpub` package closes the loop :
  - `firewallpub.EffectiveFirewall(snap, vmUUID)` walks every port
    attached to a VM, merges the effective SG list (port override OR
    network defaults), dereferences every `remote_group_uuid` to the
    concrete /32 (or /128) of every other port currently bound to
    that SG, dedups, validates. Pure ; the guest never sees group
    references.
  - `firewallpub.Publisher` reacts to existing events
    (`security_group.*`, `port.*`,
    `network.default_security_groups_updated`, `vm.created`) and
    publishes the impacted VMs' rulesets on `weft.firewall.<vm-uuid>`.
    Whole-state push, idempotent reconcile, self-healing on missed
    messages.
  - `firewallpub.StatusReceiver` decodes the reverse-direction
    `weft.firewall.*.status` wildcard the in-VM agents publish every
    10 s and re-emits each as a synthetic `firewall.status`
    PlatformEvent — so the webui's existing `/api/events` SSE pipe
    surfaces live per-VM enforcement state with no new transport.
  - Wired in cmd/weft via `startFirewallPublisher` +
    `startFirewallStatusReceiver`. No-op on the LocalEventBus.
- **Floating-IP control plane (registry + adapter + 5 gRPC handlers)**.
  The proto's FloatingIP RPCs (`AllocateFloatingIP` /
  `ReleaseFloatingIP` / `MapFloatingIP` / `UnmapFloatingIP` /
  `ListFloatingIPs`) **returned Unimplemented before this release** —
  now backed by an HCL-persisted registry. Allocator picks the next
  free address in the network's CIDR, skipping the network/broadcast
  addresses, every port-occupied IP, and the network's reserved
  gateway. Lifecycle `Allocate → Map ⇄ Unmap → Release`, idempotent
  Map on same target, refuses Release on an active FIP. Mutations
  emit `floating_ip.{allocated,released,mapped,unmapped}` events.
- **Floating-IP NAT reconciler (host-side, pure-Go nftables)**. New
  `floatingipnat` package. `LinuxReconciler` (real netlink path via
  `github.com/google/nftables`) builds the host table whole on every
  Apply :
  ```
  table ip weft-fip-nat {
    chain prerouting  { type nat hook prerouting  priority dstnat ;
      ip daddr <publicIP> dnat to <privateIP>      # per mapping
    }
    chain postrouting { type nat hook postrouting priority srcnat ;
      ip saddr <privateIP> snat to <publicIP>      # per mapping
    }
  }
  ```
  Atomic at the netlink-batch level. `StubReconciler` (darwin)
  records the desired state without touching the kernel. `Watcher`
  subscribes to `floating_ip.*` + `vm.*` + `port.*`, recomputes
  the local mappings (joining FIP mapped_to with VM port IPs), and
  drives the reconciler — wired in cmd/weft via
  `startFloatingIPNATWatcher`. IPv4-only for now ; v6 hooks land
  when an edge network is configured for v6.
- **`Adapter.ListAllPorts()`** : sorted snapshot of every port,
  used by the firewall publisher's SG-impact scan.

### Notes

- The OpenStack-parity audit (SecurityGroups + Floating IPs +
  per-tenant private networks) is now closed on the platform's
  chosen axes. The `Subnet/Bridge/VXLAN` slot is intentionally **not**
  filled : openweft's mesh-type Networks + WireGuard per-port keys
  provide L3 isolation with built-in encryption (vs VXLAN's L2
  broadcast + cleartext), and that's the architectural choice for
  cloud-native workloads. Legacy workloads needing L2 broadcast
  semantics remain available via the `weft instance` (classic VM)
  escape hatch with an operator-installed host bridge.

## [0.2.0] - 2026-06-02

v0.2.0-track work since `v0.1.0` (`8582108ab`). Roll-up of every
substantive commit, grouped by topic rather than commit order.

### Added

- **RBAC + audit log** : every `weft agent` mutation is now journaled
  to an append-only JSONL audit log (mutex-guarded, size-rotated)
  with the subject from OIDC + the verb + the target + the result.
  Reads land under the `audit` admin scope ; the webui ships a
  browser. See [`docs/operations/rbac.md`](docs/operations/rbac.md).
- **Tenant quotas — CPU / mem / vol** : per-project caps enforced at
  admission, aggregated across all project VMs. See
  [`docs/operations/tenant-quotas.md`](docs/operations/tenant-quotas.md).
- **Tenant quotas — GPU** : `gpu_count` and `gpu_memory_gib`
  dimensions wired through admission ; `RequestedGPUs` is now
  persisted on `VMInfo` so the aggregate is computed off the
  registry, not a live host scan (`255bc742d`, `2ca4fce8a`,
  `3f18e2a2d`).
- **Tenant quotas — PCI** : `pci_count` dimension covering non-GPU
  passthrough (NIC, NVMe, FPGA, sound). Same aggregate-across-VMs
  enforcement as GPU (`e7f5c6cd1`).
- **GPU end-to-end** : Host inventory + `detectGPUs` stub
  (`79ae5276e`), then real Linux detection from sysfs + nvidia-smi,
  with PCI BDF and canonical model (`8ae0d8d80`). Schedule-time
  passthrough surface on `CreateVMRequest.requested_gpus` and
  `StartVMRequest.requested_gpus`. See
  [`docs/operations/gpu-scheduling.md`](docs/operations/gpu-scheduling.md).
- **PCI passthrough** : `requested_pci` on the VM admission surface
  ; QEMU driver attaches via `-device vfio-pci`. Apple VZ explicitly
  rejects (no IOMMU passthrough surface on VZ). See
  [`docs/operations/gpu-scheduling.md`](docs/operations/gpu-scheduling.md)
  for the cross-driver capability matrix.
- **`host.cordoned`** : per-host flag flips the host out of the
  scheduler's candidate set without taking it offline. Active +
  reachable, but no new placements. Drives `weft host cordon` /
  `weft host uncordon` (`67fd017b1`).
- **Federation v0.2 lite** :
    - Data model stub : `Cluster`, `FederationManifest`, `Verifier`
      (`801923719`).
    - ed25519 `Sign` / `Verify` methods on `FederationManifest`
      (`566965122`).
    - Full lite implementation : poller, place, configfile, server,
      manifest parsing (`c0e1f71af`).
    - **Design + operator docs** :
      [`docs/design/federation.md`](docs/design/federation.md),
      [`docs/operations/federation.md`](docs/operations/federation.md).
- **Plugin RPCs (concurrent agent landing, `[Unreleased]` in
  weft-proto)** : `ListPluginCatalogue`, `ListInstalledPlugins`,
  `InstallPlugin` on `WeftAgent`. Reads the `pluginstore.Manager`
  surface (on-disk catalogue + etcd-backed installed-instance
  registry) ; install drives `pluginstore.Manager.Install` with a
  deterministic `instance_uuid = hash(name, project, inputs)`.
- **Federation RPC** : `ListFederationPeers` returns the cached
  `federation.Poller` snapshot (per-peer status `live | stale |
  unreachable`, region, weight, last-seen, last-error). Read of the
  local snapshot — no remote pull on the hot path.
- **Pluginstore + catalogue** :
    - Catalogue parser supports `count = input.<N>` on `vm` and
      `volume` blocks (`016fb6b8a`).
    - **3 HA runner plugins** : github, gitlab, forgejo
      (`a4f7b0a01`). Docs under
      [`docs/catalogue/{github,gitlab,forgejo}-runners-ha.md`](docs/catalogue/).
    - **jupyterhub-ha** : manifest + custom Spawner
      (`e0bd23cac`), user-image build context + publish workflow
      (`6ae777774`), parallelised admin bulk-stop via thread pool
      (`1bb2fd7ab`). Docs :
      [`docs/catalogue/jupyterhub-ha.md`](docs/catalogue/jupyterhub-ha.md).
    - **5 HA platform plugins** : postgres-ha, redis-ha, versitygw-ha,
      vault-ha, caddy-edge (`d548f739b`). Docs under
      [`docs/catalogue/`](docs/catalogue/).
- **Reproducible builds + supply chain** :
    - Bit-reproducible Go build, tar + OCI image with `SOURCE_DATE_EPOCH`
      pinned (`0de914bd1`).
    - Syft SBOM + SLSA L3 provenance attestation on the published
      image (`b06bfb90a`).
    - See [`docs/operations/reproducible-builds.md`](docs/operations/reproducible-builds.md),
      [`docs/operations/cosign-verify.md`](docs/operations/cosign-verify.md).
- **Validation playbook + smoke scripts** : post-deploy playbook +
  9 smoke scripts targeting real clusters (auth, scheduling,
  volumes, mesh, quotas, GPU, PCI, federation, audit)
  (`2c538fd42`). See
  [`docs/operations/validation-playbook.md`](docs/operations/validation-playbook.md).
- **Day-0 walkthrough** : production 3-DC bring-up walkthrough
  end-to-end (`8582108ab`). See
  [`docs/getting-started/production-3host.md`](docs/getting-started/production-3host.md).
- **Operator runbooks** :
    - Disaster recovery cold-start
      ([`docs/operations/disaster-recovery.md`](docs/operations/disaster-recovery.md))
      (`fe9954516`).
    - Rolling upgrade for 3-DC weft-agent
      ([`docs/operations/upgrade.md`](docs/operations/upgrade.md))
      (`3d70c0469`).
    - SSO recipes — Keycloak, Okta, Auth0
      ([`docs/operations/sso/`](docs/operations/sso/)) (`a8914ab51`).
- **Off-host snapshot backup target** : abstraction layer for
  uploading snapshots off-host (`a5a0d46f6`). Concrete S3 backend +
  runbook are TODO under `docs/operations/backup.md`.
- **`weft completion` subcommand** : bash / zsh / fish / powershell
  completions emitted from cobra (`e2558a537`).
- **BSD 3-Clause LICENSE** (`85b11a70d`).
- **Architecture overview** :
  [`docs/design/architecture.md`](docs/design/architecture.md) — the
  high-level mental map.

### Changed

- `go.mod` bumped weft-proto v0.2.0 → v0.4.0, wires
  `RequestedGpus` / `RequestedPci` on the admission surface
  (`28b511510`).
- Federation operator docs marked `design only, v0.1.0` then re-flowed
  to the v0.2 implementation timeline (`c28ab32fe`, `68b030589`).
- `CHANGELOG.md` `[Unreleased]` cut to `[0.1.0]` for the previous
  release (`53570ff6d`).

### Fixed

- No bug-fix-only commits in this window ; correctness fixes for the
  v0.2.0 track land under the WIP branches and roll into the
  follow-on patch releases.

## [0.1.0] - 2026-05-31

### Added

- VolumeSnapshot RPCs implementation (reflink-backed CoW snapshots).
- Multi-driver per-host capability: agent launches N driver plugins, scheduler honours the capability list.
- `weft up` pushes `/etc/weft/weft.hcl` to each host from the cluster-level `cluster.hcl`.
- `weft down` — convergent cluster teardown command.
- Prometheus `/metrics` endpoint plus gRPC server/client interceptors.
- Proxy plane: embedded Caddy supervised by `weft-agent`, etcd watcher, shared cert storage, `--proxy` flag.
- HCL `proxy` block in `weft.hcl` (CLI > env > HCL precedence) drives the proxy plane.
- Per-VM CLI groups under `weft instance`: `property`, `uefi`, `sshkey`.
- `weft script` and `weft flavor` subcommand groups.
- Cluster-wide catalogues with etcd-backed registries: flavors (4 RPCs), provisioning scripts (4 RPCs).
- Per-VM registries with RPCs: properties (3), UEFI NVRAM vars (3), SSH-keys (3).
- Embedded etcd (`embed.Etcd`) backend for single-host operator deploys.
- Agent `--tcp-listen` flag plus `tcp:` dial prefix for cross-host bring-up.
- Agent `--az` / `--rack` flags propagate placement metadata to the host registry.
- Cluster bring-up fetches the microVM kernel from an OCI artifact and pulls rootfs onto hosts pre-placement.
- Cluster ships infra `plan.hcl` to each host (k0sctl `files:` analog).
- Cloud-init reference template for weft host bring-up under `examples/`.
- Operations docs: RBAC model, HA failover runbook (3-DC), etcd backup + restore runbook.

### Changed

- `VzdService` renamed to `WeftAgent`; vendor refreshed against weft-proto + weft-microvm.
- Storage module `etcd3` renamed to `etcd`.
- `selfRegisterHost` reads `WEFT_HYPERVISOR`; redundant `placement{}` dropped.
- `EnsureImage` made idempotent; `copyTree` is symlink-aware.
- `RegisterMicroVM` is idempotent on re-registration.
- Cluster bring-up detaches the agent so `weft up --apply` no longer hangs.

### Fixed

- Vendor pickup of weft-microvm `docker.io/` rewrite fix.
- microVM tests: dropped busybox, seed correct cache path, force `NCL_NO_AUTO_PULL`.
- Stale `apply.go` reference in `agent/proxy/doc.go`; etcd-storage TODO anchored.

### Removed

- Legacy refs (comments, env vars, test markers, internal helpers) scrubbed.
