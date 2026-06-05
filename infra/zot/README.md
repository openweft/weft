# zot micro-VM

OCI Distribution registry. Holds every artefact the platform
produces or consumes : cloud-boot's kernel/initrd/plan blobs, the
windows-stub PE binaries, dev-VM raw disks (operator-side per the
EULA, see `cloud-boot/windows-image/`), arbitrary user push'd
images.

## Sync mirroring across DCs

Each DC's zot is configured with `extensions.sync.registries` (see
[plan.hcl](plan.hcl)) pointing at the **other two** DCs' zot
endpoints. Pull-on-demand : the first user in DC-2 pulling
`cloud-boot/loader:latest` triggers zot-dc2 to fetch the manifest
+ blobs from zot-dc1 if it's not already cached locally. Storage
is then DC-local.

For mandatory pre-population (e.g. images needed for cluster
bootstrap), `extensions.sync.content` rules can list explicit
ref globs that zot pulls eagerly on a schedule.

## Storage backends

| Backend | Where | Use case |
| --- | --- | --- |
| local-fs | per-VM volume (`/var/lib/zot`, 256 GiB) | dev, single-host, air-gapped lab |
| S3-compatible | object store (versitygw / AWS S3 / GCS / Ceph RGW) | production at scale |

The plan ships local-fs by default for simplicity. Production
deploys override at `weft infra deploy zot --storage=s3
--s3-endpoint=… --s3-bucket=…`.

## Bearer auth via dex

The `http.auth.openID.providers.dex` block (see plan.hcl) wires
zot to validate bearer tokens against dex's JWKS endpoint.
Concretely :

1. `docker push zot.<base-domain>/team-alpha/myimage:v1` from the
   user's workstation.
2. Docker client doesn't have credentials → 401 from zot →
   challenged with `Bearer realm="https://dex.<base-domain>"`.
3. User runs `weft login`, gets a dex token, exports it as
   DOCKER_PASSWORD ; retry succeeds.

ACLs on which user can push where are layered on top of dex's
group claims : `groups: ["team-alpha"]` allows `team-alpha/*`
push, `groups: ["admin"]` allows everything. The mapping lives
in zot's `accessControl` config (not shown in the bootstrap
plan — added after self-promote).

## Pulling from zot during bootstrap

Chicken-and-egg : the cluster comes up by pulling etcd / dex /
zot images from **upstream registries** (quay.io, ghcr.io). Once
zot itself is up, subsequent deploys can pull from zot via
`extensions.sync.onDemand` — the upstream image gets cached
through zot on first pull.

After self-promote, the upstream registry can become an air-gap
mirror direction : zot's `sync` source becomes a one-way feed
from upstream → local zot, and user-facing pulls hit the local
zot only.

## Plan source

[plan.hcl](plan.hcl)
