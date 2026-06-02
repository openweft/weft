# Weft plugin catalogue

The catalogue ships ready-made HA topologies as declarative `plugin.hcl`
manifests. One CLI call provisions the whole stack — networks, security
groups, micro-VMs spread across the three datacentres — through the
existing weft RPCs. No new data plane, no parallel scheduler : the
framework is glue.

## Model

A plugin is a directory under `catalogue/<name>/` containing a single
`plugin.hcl` file. The manifest declares :

- **inputs** — operator-supplied values (registration tokens, URLs,
  replica counts). `required = true` inputs must be passed on the
  command line ; secrets are masked in `plugin status`.
- **networks** — virtual networks the plugin owns. Created with
  `weft network create` semantics.
- **security_groups** — egress-only by default for runner-style
  plugins, automatically attached to the listed networks via
  `SetNetworkDefaultSecurityGroups`.
- **vms** — one or more tiers. Each `vm "<name>" {}` block expands into
  `replicas` micro-VMs with hard anti-affinity across the chosen axis
  (`az = "different"` for the 3-DC layout). Per-replica volumes are
  declared inline via nested `volume "<name>" {}` blocks. Both `vm`
  and `volume` blocks accept an optional `count` attribute — either a
  positive integer literal (`count = "4"`) or an input reference
  (`count = input.volumes_per_node`) — that materialises N copies
  named `<base>-0`..`<base>-(N-1)` at install time (default 1, no
  suffix). `minio-ha` wires `volumes_per_node` this way.

Resource names are mangled with the per-install **instance UUID** so
multiple installs of the same plugin in different projects don't
collide. The UUID is deterministic in `(plugin name, project, inputs)`,
which makes `weft plugin install` idempotent : the second run with the
same arguments is a no-op.

Tear-down goes in strict reverse order : VMs → volumes → security
groups → networks. Failed installs auto-rollback ; successful records
land in the plugin state store (`~/.local/state/weft/plugins/` by
default, etcd in production per the openweft-etcd-embedded memory).

## CLI

```
weft plugin list                                      # available + installed
weft plugin install <name> [--input k=v ...]          # apply manifest
weft plugin install <name> --dry-run --input k=v ...  # validate, no RPCs
weft plugin uninstall <name> [--instance <uuid>]      # tear down
weft plugin status [<name>] [--format json]           # what's installed
```

Flags : `--project` targets a weft project ; `--catalogue` overrides
the catalogue root ; `--state-dir` overrides the instance-state path.

## Available plugins

Grouped by `kind`. All `ha-3dc`.

| Group           | Name                  | Kind            | Description                                          |
|-----------------|-----------------------|-----------------|------------------------------------------------------|
| Runner farm     | `gitlab-runners-ha`   | runner-farm     | Three GitLab CI runners pinned one-per-AZ            |
| Runner farm     | `github-runners-ha`   | runner-farm     | Three self-hosted GitHub Actions runners             |
| Runner farm     | `forgejo-runners-ha`  | runner-farm     | Three Forgejo `act_runner` replicas                  |
| Portal          | `jupyterhub-ha`       | portal          | Per-user notebook portal ([doc](jupyterhub-ha.md))   |
| Database        | `postgres-ha`         | database        | Three Patroni-managed Postgres members with failover |
| Cache           | `redis-ha`            | cache           | Three Redis + Sentinel replicas, one per DC          |
| Object storage  | `minio-ha`            | object-storage  | Four-node erasure-coded MinIO (EC:8+8, survives a DC)|
| Secrets         | `vault-ha`            | secrets         | Three Vault members, Raft HA, KMS auto-unseal        |
| Edge proxy      | `caddy-edge`          | edge-proxy      | Three Caddy replicas at network edge, ACME TLS       |
| Observability   | `prometheus-ha`       | metrics         | Three federated Prometheus replicas ([doc](prometheus-ha.md)) |
| Observability   | `loki-ha`             | logs            | Three Loki replicas, simple-scalable, S3-backed ([doc](loki-ha.md)) |
| Observability   | `grafana-ha`          | dashboards      | Three Grafana replicas, sticky sessions, OIDC ([doc](grafana-ha.md)) |

The three runner plugins share the same shape : 3 replicas (`az = "different"`),
image `ghcr.io/openweft/weft-runner-{gitlab,github,forgejo}:v0.1.0`, dedicated
`runners` network on `10.4{2,3,4}.0.0/24`, egress-only SG (443/tcp + 22/tcp +
53/udp), one 10 GiB ephemeral cache volume per replica.

## Worked example — GitLab

```
# 1. In GitLab : Settings → CI/CD → Runners → "New project runner"
#    Copy the registration token.

# 2. From your laptop with the weft socket reachable :
weft plugin install gitlab-runners-ha \
  --project ci \
  --input registration_token=glrt-xxxxxxxxxxxxxxxxxxxx \
  --input gitlab_url=https://gitlab.com \
  --input concurrency=4

# Output:
# installed   gitlab-runners-ha   <instance-uuid>   ci

# 3. Verify the spread (replicas register as
#    gitlab-runners-ha-<short-uuid>-runner-<0|1|2>) :
weft plugin status gitlab-runners-ha
```

## Authoring a new plugin

1. Create `catalogue/<name>/plugin.hcl` wrapped in `plugin "<name>" {}`.
2. Set `version = "v1"`, `kind = "..."`, `layout = "ha-3dc"`, then
   declare `input` / `network` / `security_group` / `vm` blocks.
3. Add a one-page `docs/catalogue/<name>.md`. The catalogue index
   test (`pluginstore/manifest_test.go`) auto-parses your file as
   long as it sits under `catalogue/<name>/plugin.hcl`.

## Pull/reconcile contract

Per the `openweft_pull_model` memory : Install only issues writes
against the agent's gRPC API. `scheduling_rule` and `network` fields
on `CreateVMRequest` are recorded on the VMRecord — weft-network's
reconcile loop applies the binding asynchronously. There is no
direct agent → network push.
