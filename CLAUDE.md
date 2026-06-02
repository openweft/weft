# CLAUDE.md — weft

Project shape + cross-references for future Claude sessions on this repo.
Treat this as a navigation aid, not a spec.

## What this repo is

The single binary at the heart of weft : `weft agent` (long-lived
control-plane daemon), `weft up` (day-0 bring-up), `weft <noun> <verb>`
(operator CLI). Pure-Go core, CGO=0 on every platform ; hypervisor
specifics (Apple VZ, QEMU/KVM) live in sibling repos behind go-plugin.

See **[docs/design/architecture.md](./docs/design/architecture.md)** —
the one-stop high-level map of daemons, control flow, data plane,
catalogue, RBAC, supply chain.

## Layout, by intent

| Path | Purpose |
|---|---|
| `cmd/weft/` | cobra CLI surface, one subcommand tree per noun |
| `agent/` | gRPC server, control plane, driver dispatch, multidriver |
| `cluster/` | day-0 planner (`weft up` / `weft down`), agent config push |
| `driverplugins/` | go-plugin loader, OCI pull, per-driver event sinks |
| `federation/` | manifest + signing + poller + place ; design in [docs/design/federation.md](./docs/design/federation.md) |
| `pluginstore/` | catalogue parser + install pipeline + manifest schema |
| `catalogue/` | shipped plugins (HA platform, runners, observability, jupyterhub) |
| `imagestore/` | OCI + HTTP disk cache, reflink-aware materialiser |
| `proxy/` | embedded Caddy supervisor (see [docs/operations/proxy.md](./docs/operations/proxy.md)) |
| `sharemount/` | virtio-fs / 9p / SFTP mount fan-out |
| `tests/integration/` | end-to-end harness (`3host/`, single-host) |

## Memory cross-refs

The user keeps standing context under `~/.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/`.
Highest-value entries for this repo :

- [`project_weft_up`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/project_weft_up.md) — bring-up planner, 1→3 extensibility.
- [`project_driver_plugins`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/project_driver_plugins.md) — driver = external go-plugin, OCI-pulled, weft core stays CGO=0.
- [`openweft_etcd_embedded`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/openweft_etcd_embedded.md) — embedded etcd in single-host, external in HA.
- [`openweft_pull_model`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/openweft_pull_model.md) — cross-daemon = pull/reconcile, no synchronous push.
- [`openweft_nominal_binding`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/openweft_nominal_binding.md) — SchedulingRule nominal vs selector.
- [`project_microvm_first_strategy`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/project_microvm_first_strategy.md) — microVM is the default path ; `weft instance` is the escape hatch.
- [`project_reverse_proxy_caddy`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/project_reverse_proxy_caddy.md) — Caddy embedded, no Envoy.
- [`project_cow_clone`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/project_cow_clone.md) — reflink for VM disks, validated on btrfs+ext4.
- [`feedback_cli_cobra`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/feedback_cli_cobra.md) — every CLI uses cobra, never stdlib `flag`.
- [`feedback_no_autopublish_dev`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/feedback_no_autopublish_dev.md) — release workflows trigger on tag + dispatch only.
- [`coverage_policy`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/coverage_policy.md) — Plan B 100% on pure-Go logic, excludes generated/main/cgo.

## Recent additions

- **2026-06** — Architecture overview at
  [`docs/design/architecture.md`](./docs/design/architecture.md). Lead
  with this on any "where does X live?" question before diving into
  the source tree.
- **2026-06** — Plugin RPCs + Federation RPCs land in weft-proto
  (`ListPluginCatalogue`, `ListInstalledPlugins`, `InstallPlugin`,
  `ListFederationPeers`) and the agent surface ; CHANGELOG
  `[Unreleased]` reflects the v0.2.0-track work since `v0.1.0`.

## What's NOT here

- Mac-specific code → `weft-driver-vz` (separate repo).
- Linux QEMU code → `weft-driver-qemu` (separate repo).
- In-VM agent → `weft-microvm-agent` + `weft-microvm-init`.
- gRPC contracts → `weft-proto`.
- L7/L4 plane (DNS, LB, routers) → `weft-network`.
- UI → `weft-webui`.
- Block storage replication → `weft-block` (Longhorn fork).

## Gotchas

- Working tree commonly has WIP under `linux/` (cross-arch agent
  build) and edits to `agent/`, `driverplugins/`, `cluster/`. **Do
  not auto-stage** ; commit only the paths you explicitly touched.
- `vendor/` is committed ; bumps go through `go mod tidy && go mod
  vendor` after editing `go.mod`.
- macOS build needs the `com.apple.security.virtualization`
  entitlement on the binary ; Linux build is plain `CGO_ENABLED=0`.
