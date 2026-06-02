# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
