# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
