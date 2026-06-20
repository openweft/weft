# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `SetPodSpec` / `GetPodSpec` RPCs on `WeftAgent` : operator-facing
  surface that publishes a guestv1.PodSpec (carried as a protojson-
  encoded `spec_json` byte blob to avoid importing guestv1 into
  weft.proto). Pairs with the in-memory `Adapter.SetPodSpec` +
  `<stateDir>/podspecs.hcl` persistence — operators no longer need
  to embed the spec at boot to drive the GuestPodPlane HelloAck.
- `GetMicroVMMetrics` RPC + `MicroVMMetricsResponse` : per-VM
  telemetry snapshot (cpu / mem / net / disk + uptime). Returns the
  zero shape until the runtime-telemetry pipeline lands ; the webui
  Metrics tab renders real fields straight off the wire instead of
  falling back to Unimplemented + synthetic mock data.

### Changed (breaking)
- Rename `labels` annotation map → `properties` across the
  host/VM scheduler surface. Field numbers preserved (wire-compat
  for binary payloads). RPC + message names renamed :
  - `SetHostLabels` → `SetHostProperties`
  - `SetVMLabels` → `SetVMProperties`
  - `*Request`/`*Response` renamed in lockstep
  - `HostInfo.labels` (10) → `HostInfo.properties`
  - `VMInfo.labels` (12) → `VMInfo.properties`
  - `RegisterHostRequest.labels` (10) → `properties`
  - `HostRegistration.labels` (agent.proto, 10) → `properties`
  - `PodSpec.labels` (guest.proto, 5) → `properties`
  Single naming across the openweft stack ("properties everywhere"
  per the cluster.hcl Host.properties spec field).

## [v0.11.6] — 2026-06-14

### Added
- `PortInfo` message + `ListPortsForVM` RPC : read-only view of a
  VM's NIC bindings (MAC/IP/security-groups/created-at + reserved
  ingress/egress Mbps for the upcoming portqos persistence). Powers
  the webui Network panel and the (future) Port detail drawer.

## [v0.11.5] — 2026-06-14

### Added
- `FloatingIPInfo.rate_limit_pps` + `MapFloatingIPRequest.rate_limit_pps` :
  anti-DDoS PPS cap on inbound traffic to a floating IP. 0 means
  no cap ; >100k clamps. Persisted via the weft adapter, consumed
  by the floatingipnat reconciler on the host side.

## [v0.9.0] — 2026-06-05

### Added

- **VolumeProperty + Share extensions + Bucket + SSHKeyCatalogue +
  SchedulingRule + RegistryRemote** — closes the Tier 4-6 CLI-vs-webui
  parity gap, six new noun families on `WeftAgent` :

  - `VolumePropertyInfo` + 3 RPCs (`GetVolumeProperty` /
    `SetVolumeProperty` / `DeleteVolumeProperty`) — mirror of
    VMProperty addressed by `volume_uuid` for block volumes.
  - `ShareInfo` extensions : `GetShare` + `ResizeShare` close
    the v0.8 gap (list/create/delete already shipped).
  - `BucketInfo` + 6 RPCs (`ListBuckets` / `GetBucket` /
    `CreateBucket` / `DeleteBucket` / `GetBucketPolicy` /
    `SetBucketPolicy`) — S3 bucket catalogue ; data lives on the
    S3 endpoint (versitygw / CubeFS objectnode), the agent
    tracks credentials + mutable policy JSON.
  - `SSHKeyCatalogueEntry` + 4 RPCs (`ListSSHKeyCatalogue` /
    `AddSSHKeyCatalogue` / `RemoveSSHKeyCatalogue` /
    `ImportSSHKeyCatalogue`) — cluster-wide named SSH keys VMs
    reference at CreateVM time. Distinct from per-VM
    `weft instance sshkey`.
  - `SchedulingRuleInfo` + 4 RPCs (`ListSchedulingRules` /
    `CreateSchedulingRule` / `UpdateSchedulingRule` /
    `DeleteSchedulingRule`) — per [[openweft_nominal_binding]],
    rules carry selector + target_count + anti_affinity, the
    scheduler reconciles toward target_count.
  - `RegistryRemoteInfo` + 4 RPCs (`ListRegistryRemotes` /
    `SetRegistryRemote` (upsert) / `DeleteRegistryRemote` /
    `SearchRegistryRemote`) — OCI registry alias catalogue ;
    `credential_secret_ref` points at the secret store so the
    row never carries the raw token on the wire.

  22 new RPCs total. Mirror the existing inventory-noun pattern
  (UUID-keyed, partial-PATCH updates, cascade refusal surfaces
  blocking counts on the response when relevant).

## [v0.8.0] — 2026-06-05

### Added

- **Subnet + LoadBalancer + DNSZone + DNSRecord registries on `WeftAgent`**
  — closes the last Tier-3 CLI-vs-webui parity gap by exposing four
  network-plane noun families that were previously webui-only :

  - `SubnetInfo` + 5 RPCs (`ListSubnets` / `GetSubnet` /
    `CreateSubnet` / `UpdateSubnet` / `DeleteSubnet`) — per-network
    IP scopes, parent is `network_uuid`, immutable CIDR, mutable
    gateway + dns_servers. Update carries a `clear_dns_servers`
    bool to disambiguate "keep" vs "clear" on the wire.
  - `LoadBalancerInfo` + 6 RPCs (`ListLoadBalancers` /
    `GetLoadBalancer` / `CreateLoadBalancer` / `UpdateLoadBalancer`
    / `SetLoadBalancerBackends` / `DeleteLoadBalancer`) — project-
    scoped VIPs with `protocol` ∈ {`l4_tcp`, `l4_udp`, `l7_http`,
    `l7_https`} and a `LBBackend{address, weight}` repeated list.
    `SetLoadBalancerBackends` replaces the list atomically (clients
    GET-modify-PUT for single-entry adds/removes). `DeleteLoadBalancer`
    refuses while a FloatingIP still maps to the VIP and surfaces
    `blocked_by_fips` so the operator unmaps it first.
  - `DNSZoneInfo` + 5 RPCs (`ListDNSZones` / `GetDNSZone` /
    `CreateDNSZone` / `UpdateDNSZone` / `DeleteDNSZone`) —
    authoritative apex per project. SOA email + default TTL are
    mutable ; the zone's `records` count is server-derived.
    `DeleteDNSZone` refuses while records still attach and surfaces
    `blocked_by_records`.
  - `DNSRecordInfo` + 4 RPCs (`ListDNSRecords` / `CreateDNSRecord`
    / `UpdateDNSRecord` / `DeleteDNSRecord`) — zone children with
    `type` ∈ {`A`, `AAAA`, `CNAME`, `MX`, `TXT`, `SRV`}, optional
    per-record TTL, MX/SRV priority.

  20 new RPCs total. Mirror the existing inventory-noun pattern
  (UUID-keyed, partial-PATCH updates, cascade refusal surfaces
  blocking counts on the response).

  Why proto and not webui-only : the parity audit identified these
  four ressources as Tier 3 because the CLI couldn't drive
  end-to-end network plumbing without them. With the registry in
  the control plane, `weft subnet create`, `weft loadbalancer
  set-backends`, `weft dns-zone create`, `weft dns-record create`
  all flow through the same Unix socket as every other `weft <noun>
  <verb>` ; the webui can either continue holding its local view or
  migrate onto the live RPC at its own pace.

## [v0.7.0] — 2026-06-05

### Added

- **AvailabilityZone + Rack registry on `WeftAgent`** — elevates AZ
  and Rack from webui-only persistence (previously
  `resourceByID["azs"|"racks"]`) to first-class control-plane RPCs
  so the CLI + every other client reaches the same source of truth.
  - `AZInfo` message : uuid, code (immutable short id), name,
    region, status, created-at, server-derived racks + hosts
    counts.
  - `RackInfo` message : uuid, az_uuid (parent), code, name,
    status, height_u, created-at, server-derived hosts count.
  - 10 new RPCs : `ListAZs` / `GetAZ` / `CreateAZ` / `UpdateAZ` /
    `DeleteAZ` and `ListRacks` / `GetRack` / `CreateRack` /
    `UpdateRack` / `DeleteRack`. `Update*` are partial PATCHes
    (empty string fields = keep current). `Delete*` refuses when
    child rows still bind to the row being deleted ; the response
    surfaces the blocking-count so the operator sees exactly what
    needs draining.

  Why proto and not webui-only : the CLI parity audit surfaced
  AZ/Rack CRUD as a Tier 1 gap because the CLI couldn't drive
  bring-up of a fresh cluster. With the registry in the control
  plane, `weft az create` and `weft rack create` work over the
  Unix socket exactly like every other `weft <noun> <verb>`, and
  the webui can either continue maintaining its local view or
  migrate onto the live RPC.

## [v0.6.0] — 2026-06-05

### Added

- **Volume snapshot/backup RPC surface** on `WeftAgent` :
  - `RevertVolumeSnapshot` — rolls a block-backend volume back to a captured snapshot (driver dispatches via `weft-block` ; file-backend parents reject with FailedPrecondition).
  - `CreateVolumeBackup` / `ListVolumeBackups` / `DeleteVolumeBackup` / `RestoreVolumeBackup` — off-host backups of block-backend volumes to one of four target schemes (`oci://` recommended, `s3://` for versitygw / CubeFS objectnode, `sftp://` for sftpgo, `fs:///` for dev).
- Supporting messages : `RevertVolumeSnapshotRequest/Response`, `CreateVolumeBackupRequest/Response`, `ListVolumeBackupsRequest/Response`, `DeleteVolumeBackupRequest/Response`, `RestoreVolumeBackupRequest/Response`, `VolumeBackupInfo` (URL + volume + snapshot + project + size + state + error + created).
- `VolumeInfo.backend` (field 8) — surfaces the storage backend (`file` default, `block` for weft-block). Drives the dashboard's affordance gating on snapshot Revert + Backup actions (block-only).

## [v0.5.0] — 2026-06-02

### Added

- `ListFederationPeers` RPC on `WeftAgent` service + `FederationPeerInfo` / `ListFederationPeersRequest` / `ListFederationPeersResponse` messages — surfaces the in-process `federation.Poller` snapshot (peer name, region, weight, last-seen, classified status). Per [[openweft_pull_model]], the RPC reads the locally-cached pull state ; it does NOT trigger a remote pull on the hot path.
- `ListPluginCatalogue` + `ListInstalledPlugins` + `InstallPlugin` RPCs on `WeftAgent` service + supporting `PluginInput` / `PluginCatalogueEntry` / `PluginInstance` / request-response messages — exposes the `pluginstore.Manager` catalogue + installed-instance registry + idempotent install (returns the deterministic instance UUID).

## [v0.4.0] — 2026-06-02

### Added

- `SetHostCordoned` RPC on `WeftAgent` service + `SetHostCordonedRequest{uuid, cordoned}` / `SetHostCordonedResponse{}` messages — flips the per-host cordon flag (idempotent). Drives `weft host cordon` / `weft host uncordon`.
- `HostInfo.cordoned` (field 14) — surfaces the cordon flag in the registry. Independent of `state` ; a cordoned host stays Active + reachable but the scheduler drops it from candidate sets for new placements.
- `StartVMRequest.requested_gpus` (field 4) and `StartVMRequest.requested_pci` (field 5) — start-time passthrough requests layered on top of the VM's persisted passthrough config. Mirrors the admission-time surface added to `CreateVMRequest` / `RegisterMicroVMRequest` in v0.3.0.

## [v0.3.0] — 2026-06-02

### Added

- `GPURequest` message: `vendor`, `model`, `count`, optional `mig_slice` — mirrors the in-tree `weft/scheduling.GPURequest` struct.
- `PCIPassthroughRequest` message: `vendor_id`, `device_id`, `count` for non-GPU PCI passthrough (NIC, NVMe, FPGA, sound card).
- `CreateVMRequest.requested_gpus` (field 10) and `CreateVMRequest.requested_pci` (field 11) — admission-time passthrough shape, persisted on the VMRecord, enforced by tenant_quotas. Closes the `nil` gap noted in commit 2ca4fce8a.
- `RegisterMicroVMRequest.requested_gpus` (field 9) and `RegisterMicroVMRequest.requested_pci` (field 10) — same surface for the microVM boot path.

## [v0.2.0] — 2026-05-31

### Added

- VolumeSnapshot RPCs: `Create`, `List`, `Restore`, `Delete` (reflink-backed CoW snapshots).

## [v0.1.0] — 2026-05-30

### Added

- Initial proto schema imported from existing tree.
- `Flavors` service: cluster-wide compute catalogue RPCs (etcd-backed).
- `Scripts` service: provisioning-script catalogue RPCs (etcd-backed).
- `VMProperty` service: per-VM host-set annotation RPCs.
- `UEFIVar` service: per-VM firmware NVRAM editor RPCs.
- `VMSSHKey` service: per-VM runtime SSH-key RPCs.
- `CreateVMRequest`: `scheduling_rule` + `network` fields (pull/reconcile labels).

### Removed

- `CreateVMRequest.ssh_pub` (cloud-init era); tag 6 reserved.
