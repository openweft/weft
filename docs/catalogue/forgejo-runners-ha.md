# `forgejo-runners-ha`

Three Forgejo (`act_runner`) replicas in the HA 3-DC layout.

## What it does

- Creates a dedicated `runners` network (`10.44.0.0/24`, NAT).
- Creates a `runners-egress` security group (443/tcp, 22/tcp, 53/udp
  egress) attached to the network.
- Creates **three** micro-VMs from
  `ghcr.io/openweft/weft-runner-forgejo:v0.1.0` (2 vCPU, 4 GiB RAM,
  20 GiB root, 10 GiB cache) one per AZ.
- Each replica registers against the operator-supplied Forgejo
  instance with the one-shot token via `act_runner register`.

## Inputs

| Input                | Required | Secret | Default                              | Notes                                        |
|----------------------|----------|--------|--------------------------------------|----------------------------------------------|
| `registration_token` | yes      | yes    | —                                    | One-shot token from the admin runners page   |
| `forgejo_url`        | yes      | no     | —                                    | e.g. `https://code.example.org`              |
| `labels`             | no       | no     | `weft:docker://node:20-bullseye`     | `label:executor` pairs, comma-separated      |
| `replicas`           | no       | no     | `3`                                  | One per DC by default                        |

## Operator pre-flight

1. **Mint the registration token.**
   - Instance-wide : `Site Administration → Actions → Runners → Create new runner`.
   - Per-org : `https://<forgejo>/<org>/-/settings/actions/runners`.
   - Per-repo : `https://<forgejo>/<org>/<repo>/settings/actions/runners`.

   Copy the one-shot token — it can only be used once, so don't burn
   it on a manual `act_runner register` before running the install.

2. **Install.**

   ```
   weft plugin install forgejo-runners-ha \
     --project ci \
     --input registration_token=<token> \
     --input forgejo_url=https://code.example.org \
     --input labels="weft:docker://node:20-bullseye,gpu:docker://nvidia/cuda:12.5.0-runtime-ubuntu24.04"
   ```

3. **Verify** in the Forgejo Actions runners page — three runners
   with status `online` should appear shortly after install.

## Tear-down

```
weft plugin uninstall forgejo-runners-ha
```

The in-guest agent calls Forgejo's runner DELETE API on shutdown.
Stale offline entries (e.g. after a power loss) can be pruned from
the Forgejo admin UI.

## Labels & executors

`act_runner` understands two executor backends inside the runner VM :

- `docker` — the default, runs each job in a per-job container.
- `host` — runs jobs directly on the runner VM filesystem (faster,
  no isolation between consecutive jobs).

Pick `docker` for CI workloads with untrusted code, `host` for fully
trusted internal pipelines that need raw filesystem performance.
