# weft-agent Grafana dashboard

`weft-agent.json` is a hand-authored Grafana 10 dashboard
(schema version 38) for the `/metrics` endpoint that
`weft agent --metrics-listen=:9101` exposes. See
[../observability.md](../observability.md) for the agent-side
enabling recipe.

## Import

1. In Grafana, *Dashboards → New → Import → Upload JSON file*.
2. Pick `weft-agent.json`.
3. When prompted, wire the `datasource` variable to your
   Prometheus instance that scrapes `weft-agent:9101`. The dashboard
   declares it as a templated variable of type `prometheus`, so any
   Prometheus-flavoured datasource (vanilla Prometheus, Mimir,
   Thanos, Cortex, VictoriaMetrics' `prometheus` mode) works
   unchanged.
4. Save under whatever folder you keep platform dashboards in.

The dashboard's `uid` is `weft-agent` — if you import a second copy,
Grafana will ask you to rename it.

## Template variables

| Name | Type | Source | Purpose |
| ---- | ---- | ------ | ------- |
| `$datasource` | datasource (`prometheus`) | picker | wire to your Prom flavour |
| `$instance` | query, multi | `label_values(grpc_server_started_total, instance)` | scope by scraped agent target |
| `$grpc_service` | query, multi | `label_values(grpc_server_started_total{instance=~"$instance"}, grpc_service)` | per-RPC drill-down row |
| `$grpc_method` | query, multi | `label_values(grpc_server_started_total{instance=~"$instance",grpc_service=~"$grpc_service"}, grpc_method)` | per-RPC drill-down row |

Refresh defaults to `30s`, time range to `now-1h`.

## Panels

- *gRPC overview* — request rate by service, error ratio
  (non-OK / total), non-OK code breakdown, p50/p95/p99 handling
  latency across all RPCs.
- *Per-RPC drill-down* — request rate, p95 latency, non-OK rate,
  stream rx/tx, and approximate in-flight count, all filtered by
  `$grpc_service` / `$grpc_method`.
- *Process / Go runtime* — `process_cpu_seconds_total` rate,
  RSS + Go heap alloc, goroutines + open FDs.

## What's *not* on this dashboard

- **VM-level counters.** The agent does not yet register any
  weft-specific collectors on the `/metrics` registry — only
  process, Go runtime, and the
  `grpc_server_*` family from `go-grpc-middleware`. When
  subsystem-specific counters (scheduler, drivers, event-bus) land,
  add panels in a follow-up PR.
- **Embedded etcd stats.** `embed.Etcd` exposes its own metrics on
  its client port, not on the agent's `/metrics`. Add a separate
  scrape job (and dashboard) when you need them.
- **Caddy proxy upstreams.** The data-plane proxy ships its own
  `/metrics` on the Caddy admin endpoint — see
  [../proxy.md](../proxy.md#caddy-admin-metrics) for the
  unix-to-TCP bridge. That dashboard is a separate import.

## Metric reference (what the panels query)

All of these are produced by either `prometheus.NewProcessCollector`,
`prometheus.NewGoCollector`, or
`grpcprom.NewServerMetrics(grpcprom.WithServerHandlingTimeHistogram())`
registered in `cmd/weft/metrics.go`:

- `grpc_server_started_total`
- `grpc_server_handled_total{grpc_code}`
- `grpc_server_handling_seconds_bucket`
- `grpc_server_msg_received_total`
- `grpc_server_msg_sent_total`
- `process_cpu_seconds_total`
- `process_resident_memory_bytes`
- `process_open_fds`
- `go_goroutines`
- `go_memstats_alloc_bytes`
