# `minio-ha`

Four-node erasure-coded MinIO cluster. Three nodes spread one-per-DC
plus one extra in the largest DC so MinIO's EC math has even drive
counts.

When you want **an S3-compatible object store that survives a DC
outage** without running CephFS or Longhorn for object data.

## What it does

- Creates a dedicated `minio` network (`10.52.0.0/24`, NAT).
- Creates a `minio-storage` security group: 9000/tcp S3 + 9001/tcp
  console ingress from tenants, distributed-protocol mesh on 9000
  between nodes, 53/udp DNS egress.
- Creates **four** micro-VMs (4 vCPU, 8 GiB RAM, 20 GiB root + four
  200 GiB `drive-*` volumes mounted at `/mnt/drive-{0,1,2,3}`).
  Anti-affinity is best-effort: 3 lands one-per-AZ ; the 4th packs
  with one of the existing nodes (`host = "different"` prevents
  same-host pile-up).

## Erasure-coding math

- 4 nodes × 4 volumes = **16 drives**. MinIO's auto-EC picks **EC:8+8**
  (8 data + 8 parity), 50% storage efficiency.
- **Failure budget**: any 8 drives. A full DC outage (4 drives) plus
  any one other node still leaves objects readable.
- Drop `volumes_per_node` to 2 → 8 drives → EC:4+4, still survives
  a DC outage but with only 1 extra drive of slack. Don't go lower.

## Inputs

| Input              | Required | Secret | Default                                  | Notes                                            |
|--------------------|----------|--------|------------------------------------------|--------------------------------------------------|
| `image`            | no       | no     | `minio/minio:RELEASE.2026-05-15T00-00-00Z` | Pin a dated tag, never `latest`                  |
| `root_user`        | yes      | no     | —                                        | S3 access key id (`MINIO_ROOT_USER`)             |
| `root_password`    | yes      | yes    | —                                        | S3 secret key — MinIO refuses <8 chars           |
| `volumes_per_node` | no       | no     | `4`                                      | EC drives per node ; total = `4 × this`          |
| `volume_size_gib`  | no       | no     | `200`                                    | Per-drive size — total raw scales linearly       |
| `region`           | no       | no     | `weft-1`                                 | Surfaced in S3 GetBucketLocation                 |

## Operator pre-flight

1. **Pick root creds.** These are the cluster super-user — issue
   per-tenant access keys via the console once you're up.

2. **Size the quota.** 4 × 4 vCPU + 4 × 8 GiB RAM + 4 × 4 × 200 GiB
   = 3.2 TiB raw (1.6 TiB usable at EC:8+8) by default.

3. **Install.**

   ```
   weft plugin install minio-ha \
     --project storage \
     --input root_user=admin \
     --input root_password=$MINIO_PASS \
     --input volume_size_gib=500
   ```

## Verify

```
mc alias set weft http://minio-ha-<short>-minio-0.weft:9000 admin $MINIO_PASS
mc admin info weft          # 4 nodes Online, 16 drives Online
mc mb weft/smoke && mc cp /etc/hosts weft/smoke/hosts && mc rm weft/smoke/hosts && mc rb weft/smoke
```

## What's NOT included

- **Site-to-site replication** (`mc replicate`): no second cluster
  is provisioned. Set it up by hand if you want cross-region
  durability beyond the 3-DC footprint.
- **Multi-tenant policies**: only the root user exists. Create
  per-tenant IAM users + policies via `mc admin user add`.
- **Versioning / lifecycle**: not enabled by default. Configure
  per bucket via `mc version enable` / `mc ilm rule add`.
- **TLS**: MinIO speaks HTTPS, but ACME wiring isn't included.
  Drop certs into `/root/.minio/certs/` via a follow-up secret
  volume, or front the cluster with `caddy-edge`.
