# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
