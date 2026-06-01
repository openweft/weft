# `github-runners-ha`

Three self-hosted GitHub Actions runner replicas in the HA 3-DC layout.

## What it does

- Creates a dedicated `runners` network (`10.43.0.0/24`, NAT).
- Creates a `runners-egress` security group (443/tcp, 22/tcp, 53/udp
  egress) attached to the network.
- Creates **three** micro-VMs from
  `ghcr.io/openweft/weft-runner-github:v0.1.0` (2 vCPU, 4 GiB RAM,
  20 GiB root, 10 GiB cache) one per AZ.
- The in-guest agent uses the supplied PAT to mint short-lived
  registration tokens (via the GitHub Actions REST API) so the
  runners can self-heal without operator intervention.

## Inputs

| Input        | Required | Secret | Default                                    | Notes                                  |
|--------------|----------|--------|--------------------------------------------|----------------------------------------|
| `github_pat` | yes      | yes    | —                                          | PAT with `admin:org` or `repo:admin`   |
| `github_url` | yes      | no     | —                                          | `https://github.com/<org>` or `…/<repo>` |
| `labels`     | no       | no     | `weft,self-hosted,linux,x64`               | Comma-separated runner labels          |
| `replicas`   | no       | no     | `3`                                        | One per DC by default                  |

## Operator pre-flight

1. **Mint the PAT.** Settings → Developer settings → Personal access
   tokens → Fine-grained tokens. Scope :
   - Org-wide runners : `admin:org` (read/write).
   - Single repo runners : `repo:admin`.

   Note : a classic PAT works too, but fine-grained is the recommended
   path. The plugin only needs the org/repo scope you intend to attach
   runners to.

2. **Pick the URL.**
   - Org-level : `https://github.com/<org>`. Runners appear under
     `Settings → Actions → Runners` at the org level.
   - Repo-level : `https://github.com/<org>/<repo>`. Runners appear
     under the repo's own Actions settings.

3. **Install.**

   ```
   weft plugin install github-runners-ha \
     --project ci \
     --input github_pat=ghp_XXXXXXXXXXXXXXXXXXXX \
     --input github_url=https://github.com/openweft \
     --input labels=weft,self-hosted,linux,x64,gpu
   ```

4. **Verify** in `Settings → Actions → Runners` : three runners with
   status `idle` and the labels you passed.

## Tear-down

```
weft plugin uninstall github-runners-ha
```

The in-guest agent calls `DELETE /actions/runners/<id>` on shutdown so
the org/repo runner list stays clean. If a VM is force-killed, the
runner appears as `offline` in GitHub — use the GitHub UI to remove
the stale entry.

## Security notes

The PAT is stored in the per-VM property surface as
`env.GH_RUNNER_TOKEN` — only the in-guest runner agent reads it. The
property store is encrypted at rest in the etcd backend (per the
storage policy in `docs/operations/observability.md`).
