# JupyterHub HA — operator guide

A weft catalogue plugin that exposes a multi-AZ JupyterHub
control plane with **one microVM per user**. Notebooks are
fully isolated (kernel-level, not container namespace), the
Hub runs as 3 replicas behind the Caddy proxy plane, and OIDC
auth federates to whichever issuer weft itself trusts.

## What you get

- `hub.<domain>` answers from 3 controller VMs, one per DC,
  routed by Caddy with `cookie:jupyterhub-session-id` stickiness.
- Login spawns `vm-jh-<user>` with a per-user persistent volume
  mounted at `/home/jovyan`. Stop = idle-cull. Restart = resume.
- DB defaults to **CockroachDB** (3-node), with a documented
  `sqlite-on-NFS` escape hatch for sub-100-user deploys.

## Prerequisites

1. **A weft project for user VMs**, with quotas sized for the
   user population. Per `docs/operations/tenant-quotas.md`,
   each user consumes `cpu_per_user` cores and
   `memory_gib_per_user` GiB, so for 100 users at defaults
   (2 vCPU, 4 GiB) set the project's cap to ≥ 200 cores and
   ≥ 400 GiB. **If you don't, `weft instance create` returns
   `codes.ResourceExhausted` on spawn — the spawner surfaces
   that as an HTTP 503 with a clear message, but the operator
   pain stays real.**
2. **OIDC client** registered with your issuer with redirect
   URI `https://<domain>/hub/oauth_callback`.
3. **Caddy / `weft-agent --proxy` enabled** on the cluster ;
   the plugin emits a `proxy_route "hub"` block which the
   proxy plane consumes (see `docs/operations/proxy.md`).
4. **Agent socket exposed to controllers.** The Hub VM needs to
   shell `weft instance ...` ; the plugin manifest's
   `share { tag = "weft-sock" }` block mounts the host's
   `/var/run/weft` at the controller's `/run/weft`. On macOS
   hosts the share is virtio-9p (per the project's
   `qemu_microvm_9p` constraint) ; on Linux it's virtio-fs.
   This is a **same-host model** — controllers must run on
   weft hypervisors, not on third-party k8s.

## Install

Today (until the framework agent's `weft plugin install` lands):

```bash
# 1. create the user-VM project + quotas
weft project create --name jupyter --uuid 11111111-aaaa-bbbb-cccc-222222222222
weft tenant quota set 11111111-aaaa-bbbb-cccc-222222222222 \
     --cpu-count 200 --memory-gib 400 --volume-gib 5000

# 2. install the plugin manifest
weft plugin install catalogue/jupyterhub-ha \
     --input oidc_issuer=https://dex.example.com \
     --input oidc_client_id=jupyterhub \
     --input oidc_client_secret=$JH_CLIENT_SECRET \
     --input domain=hub.example.com \
     --input project_uuid=11111111-aaaa-bbbb-cccc-222222222222 \
     --input user_group=weft:project:11111111-aaaa-bbbb-cccc-222222222222
```

Output: 3 controller VMs (`vm-jupyterhub-controller-1/2/3`),
3 cockroach VMs (skip with `--input db_backend=sqlite-nfs`),
a `jupyterhub-control` network, 2 security groups, and a Caddy
route for `hub.example.com`.

## Inputs

| Input                 | Default                                          | Notes                                                |
|-----------------------|--------------------------------------------------|------------------------------------------------------|
| `oidc_issuer`         | —                                                | required                                             |
| `oidc_client_id`      | —                                                | required                                             |
| `oidc_client_secret`  | —                                                | required, stored in the cluster secret store         |
| `domain`              | —                                                | required (e.g. `hub.example.com`)                    |
| `project_uuid`        | —                                                | required ; the project owning user VMs               |
| `image`               | `quay.io/jupyter/minimal-notebook:python-3.12`   | stopgap — see "Building the user image" below       |
| `cpu_per_user`        | `2`                                              | vCPU per user notebook VM                            |
| `memory_gib_per_user` | `4`                                              | RAM (GiB) per user notebook VM                       |
| `home_volume_gib`     | `50`                                             | `/home/jovyan` volume, reflink-snapshottable        |
| `idle_minutes`        | `60`                                             | stop (not destroy) after N idle minutes              |
| `db_backend`          | `cockroach`                                      | `cockroach` \| `sqlite-nfs`                          |
| `admin_group`         | `weft:admin`                                     | OIDC group → Hub admin                               |
| `user_group`          | —                                                | OIDC group permitted to log in ; convention `weft:project:<uuid>` |

## Building the user image

**`ghcr.io/openweft/jupyter-user:v0.1.0` is a follow-up that
doesn't exist yet.** Until that's published you have two paths :

1. **Use the upstream stopgap** (the manifest default).
   `quay.io/jupyter/minimal-notebook:python-3.12` ships
   Jupyter + Python + core scientific stack. It's not optimised
   for microVM cold-start but it works.
2. **Build your own** : start `FROM quay.io/jupyter/minimal-notebook`,
   bake in your team's libraries + a `/etc/cloud-init/`-friendly
   entrypoint, push to your private registry, set
   `--input image=...`. Future versions of this plugin will ship
   a reference Dockerfile under `image/` here.

## SQLite-on-NFS alternative

For deploys under ~50 users where 3 Cockroach VMs is overkill,
pass `--input db_backend=sqlite-nfs`. The deployer skips the
cockroach VMs and the controllers fall back to a SQLite file on
a shared NFS volume (you provide the NFS — declare a `volume`
backed by your file-storage class and mount it at
`/var/lib/jupyterhub` on the controllers).

Caveat : SQLite + sticky sessions = single-writer. The Caddy
sticky cookie pins a given browser to one controller ; that
controller does all DB writes ; the other two serve reads only.
A controller failover is therefore a ~10 s window of degraded
service while the cookie re-pins. Cockroach has no such pinning.

## OIDC group mapping

Per `docs/operations/rbac.md` the convention is:

- `weft:admin` → Hub admin (override via `admin_group`).
- `weft:project:<project_uuid>` → permitted Hub user (override
  via `user_group` ; leave blank to allow any authenticated
  user).

The spawner does NOT re-check the group at every notebook
request — the Authenticator's allow-list is the gate. If a
user is removed from the group at the issuer, the Hub picks
it up on their next token refresh (default 1 h).

## Idle culling

`jupyterhub-idle-culler` runs as a Hub service and calls our
spawner's `stop()` after `idle_minutes` of no activity. The VM
goes to `stopped` ; the home volume persists ; the next login
restarts the same VM. **We deliberately don't `--remove-named-
servers`** — that would also delete the volume reference and
the user's notebooks with it.

## Per-user quotas

Tenant quotas (commit `88cece7c6`) are project-scoped, so all
users share the project's cap. If you want per-user caps you'll
need to either :

- create one project per user (heavy, but possible — set
  `project_uuid` dynamically in a fork of the spawner) ; OR
- wait for the planned per-user tenant model (see
  `docs/operations/tenant-quotas.md` "When the tenant model
  lands").

## Validation

```bash
# Python syntax gate
pkgx python3 -m py_compile \
   catalogue/jupyterhub-ha/spawner/weft_spawner.py \
   catalogue/jupyterhub-ha/jupyterhub_config.py

# HCL parse — once the framework agent ships `weft plugin validate` :
weft plugin validate catalogue/jupyterhub-ha
```

## Known limitations

- Cold-start is dominated by image pull. First login of a user
  whose VM's image isn't cached on the chosen host takes
  10–60 s ; subsequent logins on the same host are ~2–5 s.
  Pre-pull the image on every host (`weft image pull --on-all-hosts`)
  to avoid the cliff.
- No GPU support yet. The spawner doesn't pass `--gpu` ; once
  `docs/operations/gpu-scheduling.md` stabilises we'll add a
  `gpu_per_user` input.
- The Hub admin UI's "Stop all" button calls `stop()` per user
  serially. For >100 users that's slow — track upstream's
  parallel-stop work, switch to gRPC streaming when ready.
