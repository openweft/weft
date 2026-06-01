# `gitlab-runners-ha`

Three GitLab CI runner replicas in the HA 3-DC layout — one per
availability zone, no inbound, egress to `gitlab.com` or the operator's
self-hosted GitLab instance.

## What it does

- Creates a dedicated `runners` network (`10.42.0.0/24`, NAT).
- Creates a `runners-egress` security group (443/tcp, 22/tcp, 53/udp
  egress ; nothing inbound) and attaches it to the network.
- Creates **three** micro-VMs from
  `ghcr.io/openweft/weft-runner-gitlab:v0.1.0` (2 vCPU, 4 GiB RAM,
  20 GiB root, 10 GiB ephemeral cache volume) with hard anti-affinity
  across DCs via `placement { az = "different" }`.
- Each replica receives the registration token + GitLab URL through
  the per-VM property store, picked up by the in-guest runner agent.

## Inputs

| Input                | Required | Secret | Default                  | Notes                                     |
|----------------------|----------|--------|--------------------------|-------------------------------------------|
| `registration_token` | yes      | yes    | —                        | From Settings → CI/CD → Runners           |
| `gitlab_url`         | no       | no     | `https://gitlab.com`     | Self-hosted GitLab URL                    |
| `replicas`           | no       | no     | `3`                      | One per DC by default                     |
| `concurrency`        | no       | no     | `4`                      | `concurrent =` in `config.toml`           |

## Operator pre-flight

1. **Mint the registration token.** Project, group, or instance scope :
   - Project runner : `https://gitlab.com/<group>/<project>/-/settings/ci_cd`
   - Group runner : `https://gitlab.com/groups/<group>/-/runners`
   - Instance runner : `https://gitlab.com/admin/runners`

   Click **New runner**, pick the `linux` platform and any tags you want
   (`weft`, `self-hosted`, etc.), and copy the `glrt-…` token.

2. **Pick the weft project.** The plugin lives in one tenant project ;
   make sure that project has enough CPU/RAM headroom in its quota :
   3 replicas × 2 vCPU × 4 GiB RAM.

3. **Install.**

   ```
   weft plugin install gitlab-runners-ha \
     --project ci \
     --input registration_token=glrt-XXXXXXXXXXXXXXXXXXXX \
     --input gitlab_url=https://gitlab.example.com
   ```

4. **Verify** in the runners page — three runners with status
   `online` should appear within ~60 seconds of the install banner.

## Tear-down

```
weft plugin uninstall gitlab-runners-ha
```

This deletes the three runners (and their cache volumes), the
egress security group, and the `runners` network in reverse order.
The plugin instance state record is removed only on full success.

## Pull model

The runner repos publish images via `workflow_dispatch + tags: v*`
(per `feedback_no_autopublish_dev`). New runner versions land via
`weft image pull ghcr.io/openweft/weft-runner-gitlab:vX.Y.Z`
followed by a `weft plugin uninstall` + re-`install` with the bumped
image tag. There is no in-place mutation path today.
