# `prometheus-ha`

Three federated Prometheus replicas, one per DC, with TSDB persistence
and optional remote_write.

When you want **metrics that survive a DC outage** without spinning up
Cortex or Mimir from scratch.

## What it does

- Creates a dedicated `metrics` network (`10.60.0.0/24`, NAT).
- Creates a `prometheus-metrics` security group: 9090/tcp ingress for
  Prometheus UI + `/federate`, tenant-wide TCP egress for scraping
  exporters, 443/tcp egress for remote_write sinks, 53/udp DNS egress.
- Creates **three** Prometheus replicas (4 vCPU, 8 GiB RAM, 20 GiB
  root + a 200 GiB TSDB volume mounted at `/prometheus`) with hard
  anti-affinity (`az = "different"`, `host = "different"`).

## Federation model

The 3 replicas scrape the same target set independently and stamp
samples with `cluster = <external_labels_cluster>` (same on all 3)
plus `replica = <0|1|2>` (different per replica, in-guest agent
stamps it). Downstream (Grafana, remote_write receivers) dedupes on
`(cluster, job, instance)` and ignores the replica axis. A DC outage
drops 1 replica ; the other 2 keep scraping and the downstream sees
no gap.

## Inputs

| Input                     | Required | Secret | Default              | Notes                                              |
|---------------------------|----------|--------|----------------------|----------------------------------------------------|
| `image`                   | no       | no     | `prom/prometheus:v2.55` | `ghcr.io/openweft/prometheus-ha:v0.1.0` not yet published |
| `external_labels_cluster` | yes      | no     | —                    | e.g. `prod-eu-west-1`. Stamps every sample         |
| `scrape_interval`         | no       | no     | `30s`                | Global ; per-job overrides via in-guest config     |
| `retention`               | no       | no     | `15d`                | Local TSDB retention                               |
| `remote_write_url`        | no       | no     | `""`                 | Empty disables remote_write                        |
| `tsdb_volume_gib`         | no       | no     | `200`                | Per-replica `/prometheus` volume                   |
| `tenant_network_cidrs`    | no       | no     | `10.0.0.0/8`         | Narrow 9090 ingress in production                  |

## Operator pre-flight

1. Pick a stable `external_labels_cluster` (e.g. `prod-eu-west-1`).
2. Optional : set `remote_write_url` for long-term history (Mimir,
   VictoriaMetrics, Grafana Cloud). Otherwise local TSDB only.
3. Size project quota : 3 × 4 vCPU + 3 × 8 GiB RAM + 3 × 200 GiB.
4. Install :

   ```
   weft plugin install prometheus-ha --project observability \
     --input external_labels_cluster=prod-eu-west-1 \
     --input retention=30d \
     --input remote_write_url=https://mimir.example.com/api/v1/push
   ```

## Verify

```
# Each replica answers on 9090.
curl http://prometheus-ha-<short>-prometheus-0.weft:9090/-/healthy
# Expect: Prometheus Server is Healthy.

# Confirm all 3 replicas see the same targets.
for i in 0 1 2; do
  curl -s http://prometheus-ha-<short>-prometheus-$i.weft:9090/api/v1/targets \
    | jq '.data.activeTargets | length'
done
```

## What's NOT included

- **Alertmanager** : separate plugin (pending) — without it you can
  author alert rules but nothing notifies.
- **Service discovery** : seeds with static-config + cluster `/sd/weft` ;
  add Kubernetes/EC2/Consul SD by hand.
- **TLS / mTLS** : 9090 plain HTTP — front with `caddy-edge`.
- **PromQL access controls** : any client on `tenant_network_cidrs`
  can query ; use Grafana datasource proxy + OIDC for per-user limits.
