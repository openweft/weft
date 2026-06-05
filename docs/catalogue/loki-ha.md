# `loki-ha`

Three Loki replicas in **simple-scalable-mode** (all-in-one binary
behind a load balancer), one per DC, backed by S3-compatible chunk
storage.

When you want **a log store that survives a DC outage** without the seven-microservice Loki layout.

## What it does

- Creates a dedicated `logs` network (`10.61.0.0/24`, NAT).
- Creates a `loki-logs` security group: 3100/tcp ingress for the
  Loki HTTP API (push + query), 7946/tcp memberlist gossip between
  replicas, 9095/tcp inter-component gRPC, 443 / 9000 egress for S3,
  53/udp DNS egress.
- Creates **three** Loki replicas (4 vCPU, 8 GiB RAM, 20 GiB root +
  a 50 GiB `/var/loki` volume for the boltdb-shipper compactor cache)
  with hard anti-affinity (`az = "different"`).

## Simple-scalable mode trade-off

Loki ships microservices-mode (7+ roles : distributor, ingester,
querier, query-frontend, compactor, ruler, index-gateway — scales
past 10 TB/day, fleet balloons) and simple-scalable (one
`loki -target=all` binary per VM — tops out ~1 TB/day per replica).
This plugin picks simple-scalable ; fork it and split into
`-target=read` + `-target=write` if you outgrow the limit.

## Inputs

| Input                  | Required | Secret | Default                | Notes                                              |
|------------------------|----------|--------|------------------------|----------------------------------------------------|
| `image`                | no       | no     | `grafana/loki:3.3`     | Upstream `grafana/loki` ; no openweft fork         |
| `retention_days`       | no       | no     | `30`                   | Compactor retention window                         |
| `replication_factor`   | no       | no     | `3`                    | Loki ingester RF. **MUST be ≤ 3** (VM count)       |
| `s3_endpoint`          | yes      | no     | —                      | e.g. `http://minio.weft:7070`                      |
| `s3_bucket`            | yes      | no     | —                      | Pre-create ; Loki won't auto-provision             |
| `s3_access_key`        | yes      | yes    | —                      | S3 access key id                                   |
| `s3_secret_key`        | yes      | yes    | —                      | S3 secret access key                               |
| `s3_region`            | no       | no     | `weft-1`               | Arbitrary value works for versitygw                    |
| `cache_volume_gib`     | no       | no     | `50`                   | Per-replica `/var/loki`                            |
| `tenant_network_cidrs` | no       | no     | `10.0.0.0/8`           | Narrow 3100 ingress in production                  |

## Operator pre-flight

1. Provision S3 (use `catalogue/versitygw-ha` if you don't have one) :
   ```
   mc mb weft/loki-chunks
   mc admin user svcacct add weft admin --access-key=loki --secret-key=$SK
   ```
2. Size project quota : 3 × 4 vCPU + 3 × 8 GiB RAM + 3 × 50 GiB.
3. Install :

   ```
   weft plugin install loki-ha --project observability \
     --input s3_endpoint=http://versitygw-ha-<short>-versitygw-0.weft:7070 \
     --input s3_bucket=loki-chunks \
     --input s3_access_key=loki --input s3_secret_key=$SK
   ```

## Verify

```
curl http://loki-ha-<short>-loki-0.weft:3100/ready   # Expect: ready
NOW=$(date +%s%N)
curl -s -H 'Content-Type: application/json' \
  -d "{\"streams\":[{\"stream\":{\"job\":\"smoke\"},\"values\":[[\"$NOW\",\"hello\"]]}]}" \
  http://loki-ha-<short>-loki-0.weft:3100/loki/api/v1/push
curl -s "http://loki-ha-<short>-loki-1.weft:3100/loki/api/v1/query?query={job=\"smoke\"}" | jq
```

## What's NOT included

- **Ruler / alerting** : not enabled — Alertmanager is a separate
  plugin (pending), same story as `prometheus-ha`.
- **Multi-tenant auth** : `X-Scope-OrgID` defaults to `fake` ; wire
  per-tenant headers via Grafana proxy or `caddy-edge` + JWT.
- **TLS** : 3100 plain HTTP — front with `caddy-edge`.
- **Compactor tuning** : defaults to hourly ; adjust the in-guest
  config seed for >>1 TB/day.
