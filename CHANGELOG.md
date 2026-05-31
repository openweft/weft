# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
