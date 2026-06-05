# `versitygw-ha`

Three-node S3-compatible gateway in the 3-DC HA layout, one replica
per datacenter. Replaces the previous `minio-ha` plugin (removed
2026-06 per the openweft no-AGPL policy — see memory
[`feedback_no_minio`](../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/feedback_no_minio.md)).

## Why versitygw

[versitygw](https://github.com/versity/versitygw) (Versity Software,
Apache-2.0) is a pure S3-protocol gateway over POSIX backends. It
gives us:

- A fully open, redistributable S3 implementation without the
  AGPLv3 + commercial dual-license trap MinIO ships.
- Independence from any storage layout — durability is decoupled
  from the gateway and provided by either weft-block (replicated
  controllers per drive) or a CubeFS shared mount, both of which
  weft already ships as first-class storage primitives.
- A small attack surface : no embedded console, no inter-node
  distributed protocol, no autotuned erasure coding to keep an eye
  on. The gateway is stateless ; restarting any replica is a no-op.

## Layout

| Node             | DC     | Notes                                          |
| ---------------- | ------ | ---------------------------------------------- |
| `versitygw-0`    | DC-A   | Anti-affinity at AZ + host level               |
| `versitygw-1`    | DC-B   | Same                                           |
| `versitygw-2`    | DC-C   | Same                                           |

Surviving a full DC outage = two replicas keep serving requests.
Read scaling is linear with the replica count when the L7 Caddy in
weft-agent hashes requests by `bucket+key` (the default).

## Inputs

| Input                | Required | Secret | Default                                    | Notes                                                  |
| -------------------- | -------- | ------ | ------------------------------------------ | ------------------------------------------------------ |
| `image`              | no       | no     | `ghcr.io/versity/versitygw:v1.0.13`        | Pin to a dated tag, never `latest`                     |
| `root_access_key`    | yes      | no     | —                                          | S3 access-key-id                                       |
| `root_secret_key`    | yes      | yes    | —                                          | S3 secret-access-key                                   |
| `backend`            | no       | no     | `block`                                    | `block` = per-replica weft-block volumes ; `cubefs` = shared CubeFS mount |
| `cubefs_share`       | no       | no     | (empty)                                    | Required when `backend=cubefs`                         |
| `volumes_per_node`   | no       | no     | `4`                                        | Block volumes per replica (`backend=block` only)       |
| `volume_size_gib`    | no       | no     | `200`                                      | Inert when `backend=cubefs`                            |
| `region`             | no       | no     | `weft-1`                                   | S3 region label, free-form                             |

## Operator pre-flight

1. Pick a root S3 access-key + secret-key and stash them in the
   cluster secret store (never in plain HCL).

2. Decide on the backend :
   - `block` (default) : 4 volumes × 3 replicas = 12 block volumes
     materialised at install time, each replicated by weft-block's
     controller chain. Durability scales with the block volume's
     replica factor.
   - `cubefs` : the operator created a CubeFS share separately
     (e.g. via the `cubefs-ha` plugin or an existing volume) and
     passes its name as `cubefs_share`. Every replica mounts the
     same path at `/data/shared`.

3. Install the plugin :

   ```sh
   weft plugin install versitygw-ha \
     --project storage \
     --input root_access_key=AKIA... \
     --input root_secret_key=$VW_SECRET \
     --input backend=block
   ```

## Client-side configuration

Once the install converges :

```sh
aws --endpoint-url http://versitygw-ha-<short>.weft:7070 \
    s3 ls
```

The S3 endpoint is a stable in-cluster DNS name fronting the three
replicas. Operators wanting an external HTTPS edge route the L7
proxy in front of it (see [`caddy-edge`](caddy-edge.md)).

## What this plugin does NOT do

- **Erasure coding inside the gateway.** versitygw is a stateless
  protocol translator ; durability is whatever the backend
  provides. For replicated-everywhere semantics, leave
  `backend=block` and let weft-block's controller chain handle the
  replica fan-out.
- **Bucket-level lifecycle rules / object versioning configuration.**
  Manage these via the standard `s3api put-bucket-*` calls after
  install — versitygw honours the bucket-side metadata directly.
- **TLS termination.** versitygw speaks plain HTTP on 7070 ; route
  through the L7 Caddy edge plugin for ACME-managed TLS.

## Migration from `minio-ha`

The old `minio-ha` plugin shipped `volumes_per_node × 4 nodes`
EC drives. `versitygw-ha` ships `volumes_per_node × 3 replicas`
plain volumes — semantically simpler but slightly different sizing.
If your install was sized for `minio-ha`'s 16-drive EC:8+8 layout
(64 GiB net per 200 GiB raw, 8/16), the equivalent `versitygw-ha`
sizing is `volumes_per_node=4` with `backend=block` and the block
driver's replica factor set to match your prior durability target.
