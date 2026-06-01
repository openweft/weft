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
  declared inline via nested `volume "<name>" {}` blocks.

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

| Name                  | Kind         | Layout  | Description                                        |
|-----------------------|--------------|---------|----------------------------------------------------|
| `gitlab-runners-ha`   | runner-farm  | ha-3dc  | Three GitLab CI runners pinned one-per-AZ          |
| `github-runners-ha`   | runner-farm  | ha-3dc  | Three self-hosted GitHub Actions runners           |
| `forgejo-runners-ha`  | runner-farm  | ha-3dc  | Three Forgejo `act_runner` replicas                |
| `jupyterhub-ha`       | portal       | ha-3dc  | Per-user notebook portal *(written by sibling agent — see [jupyterhub-ha.md](jupyterhub-ha.md))* |

The three runner plugins all share the same shape :

- **Replicas = 3**, one per DC (`az = "different"` in the scheduling
  rule). Bump with `--input replicas=N`.
- Image `ghcr.io/openweft/weft-runner-{gitlab,github,forgejo}:v0.1.0`
  from the matching runner repo.
- Dedicated `runners` network on `10.4{2,3,4}.0.0/24`, isolated from
  tenant networks.
- Egress-only security group : 443/tcp (CI hub), 22/tcp (git+ssh),
  53/udp (DNS). No inbound rules.
- One 10 GiB ephemeral cache volume per replica.

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

# 3. Verify the spread :
weft plugin status gitlab-runners-ha

# 4. Three replicas register under the runner name pattern
#    gitlab-runners-ha-<short-uuid>-runner-<0|1|2>. In the GitLab
#    runners page they show up tagged with the labels you pinned
#    via `--input labels=...`.
```

## Authoring a new plugin

1. Create `catalogue/<name>/plugin.hcl`.
2. Wrap everything in a `plugin "<name>" { ... }` block.
3. Set `version = "v1"`, `kind = "<runner-farm|portal|...>"`,
   `layout = "ha-3dc"`.
4. Declare `input`, `network`, `security_group`, `vm` blocks as
   shown in any of the runner manifests.
5. Add a one-page `docs/catalogue/<name>.md` describing the inputs and
   the operator-side prerequisites.
6. The catalogue index test (`pluginstore/manifest_test.go ::
   TestParseCatalogue_AllShippedPluginsParse`) auto-verifies your
   manifest as long as it sits under `catalogue/<name>/plugin.hcl`.

## Pull/reconcile contract

Per the `openweft_pull_model` memory : Install only issues writes
against the agent's gRPC API. `scheduling_rule` and `network` fields
on `CreateVMRequest` are recorded on the VMRecord — weft-network's
reconcile loop applies the binding asynchronously. There is no
direct agent → network push.
