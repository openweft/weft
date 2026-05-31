# Observability (`weft agent --metrics-listen`)

`weft-agent` exposes two Prometheus-format metric surfaces, both
opt-in :

- **Control plane** — the agent's own `/metrics` endpoint, served on
  whatever `host:port` the operator passes to `--metrics-listen`.
  Carries the standard `process_*` + `go_*` runtime collectors plus
  the gRPC server-side `grpc_server_*` histograms / counters wired
  through `go-grpc-middleware/providers/prometheus`.
- **Data plane (proxy)** — Caddy's built-in `/metrics`, on the
  proxy admin socket. See
  [docs/operations/proxy.md](proxy.md#caddy-admin-metrics) for the
  enabling recipe and the unix-socket-to-TCP bridge options.

Both default to disabled — a Mac laptop running `weft agent` for
local dev pays no observability cost unless the operator flips the
knobs.

## Enabling on the agent

### HCL (recommended)

```hcl
# /etc/weft/weft.hcl (or ~/.config/weft/weft.hcl)
metrics_listen = ":9101"
```

### CLI flag

```sh
weft agent --metrics-listen=:9101
```

Precedence is the same rule as every other agent setting :
`CLI > HCL > built-in default`. The built-in default is empty
string = disabled.

The listener binds before `run()` returns, so a port-conflict on
`:9101` is loud at boot — not surfaced 10 s later in a goroutine log.

## What gets scraped

The registry behind `/metrics` is a fresh `*prometheus.Registry`
(NOT the `prometheus.DefaultRegisterer`) — explicit registries are
the recommended practice so unrelated `client_golang` users elsewhere
in the binary can't leak metrics into the agent's scrape output.

Collectors registered today :

| Source | Examples |
| ------ | -------- |
| `prometheus.NewProcessCollector` | `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds`, … |
| `prometheus.NewGoCollector` | `go_goroutines`, `go_gc_duration_seconds`, `go_memstats_alloc_bytes`, … |
| `grpcprom.ServerMetrics` (with `WithServerHandlingTimeHistogram`) | `grpc_server_started_total`, `grpc_server_handled_total{grpc_code}`, `grpc_server_handling_seconds_bucket`, `grpc_server_msg_received_total`, `grpc_server_msg_sent_total` |

The gRPC interceptor sits **after** the auth interceptor in the
chain, so authn-failed RPCs still increment
`grpc_server_handled_total{grpc_code="Unauthenticated"}`. That's the
signal you want — silent 401s would otherwise be invisible until a
user complains.

## Sample Prometheus scrape config

```yaml
# /etc/prometheus/prometheus.yml (fragment)
scrape_configs:
  - job_name: weft-agent
    scrape_interval: 15s
    static_configs:
      - targets:
          - host-a.dc1.example.com:9101
          - host-b.dc1.example.com:9101
          - host-c.dc1.example.com:9101
        labels:
          weft_role: control-plane

  - job_name: weft-proxy
    scrape_interval: 15s
    metrics_path: /metrics
    # See docs/operations/proxy.md — bridge the Caddy unix socket
    # through a sidecar socat or open a TCP admin listener first.
    static_configs:
      - targets:
          - host-a.dc1.example.com:9102
          - host-b.dc1.example.com:9102
          - host-c.dc1.example.com:9102
        labels:
          weft_role: data-plane
```

## Operator quick-checks

```sh
# All families exposed by the agent — sanity-check it's up.
curl -sS localhost:9101/metrics | head -40

# Per-RPC throughput / latency.
curl -sS localhost:9101/metrics | grep grpc_server_handled_total
curl -sS localhost:9101/metrics | grep grpc_server_handling_seconds_bucket

# Go runtime health — useful when an operator reports a hang.
curl -sS localhost:9101/metrics | grep -E '^(go_goroutines|go_gc_duration_seconds)'
```

## Grafana dashboards

We don't ship a dashboard JSON in this commit — the metric names
above are stable client_golang / grpc-ecosystem conventions, so any
community dashboard that targets them will work. Starting points :

- `Grafana.com Dashboard #9628` — the canonical "Go Processes"
  dashboard, picks up the `process_*` + `go_*` series unchanged.
- `Grafana.com Dashboard #14869` — the standard gRPC server
  dashboard built on `grpc_server_*` series ; same panel layout
  applies to weft-agent without modification.

A weft-branded dashboard with curated panels per subsystem
(scheduler, drivers, event-bus) lands in a follow-up once the
production deploys have collected a few weeks of baseline data.

## Sizing notes

- Scrape volume : at 15 s interval, expect ~30 KB / scrape after a
  few RPCs have lit up the gRPC histograms — negligible.
- Cardinality risk : `grpc_server_handled_total` has label cardinality
  proportional to `(grpc_service × grpc_method × grpc_code)` —
  bounded by the proto definition, no user-supplied label dimensions,
  so it stays well under any Prometheus cardinality budget.

## Disabling

Drop the `metrics_listen` line from HCL (or pass `--metrics-listen=""`
on the command line). The /metrics listener is not bound, the
`grpcprom.ServerMetrics` collector is not constructed, and the chain
falls back to a single auth interceptor — zero overhead, zero
allocations on the hot path.
