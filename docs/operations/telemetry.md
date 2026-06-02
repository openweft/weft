# Anonymous opt-in telemetry

**TL;DR — the openweft project does NOT collect telemetry by default
and runs NO central server.** This is a self-collected helper :
operators who want fleet stats wire their own aggregator and point a
`weft agent` at it. Nothing leaves your cluster without an explicit
`weft telemetry enable`.

## The model

- **Opt-in.** Default state is `disabled` ; zero outbound calls.
- **Anonymous.** Payload carries `sha256(cluster_uuid +
  install_date)[:16]` — stable enough to de-duplicate heartbeats,
  opaque enough that the receiver cannot reverse it.
- **Self-hosted.** No openweft endpoint. Operator supplies the URL
  via `--endpoint`. Empty endpoint = silent no-op at tick time.
- **Cadence.** 24h ticker from the agent's bootstrap goroutine ;
  first tick fires after the interval, never at boot.

## Payload

Each tick POSTs a single JSON envelope, `Content-Type:
application/json`. Exact shape :

```json
{
  "anonymous_id":      "<sha256(cluster_uuid+install_date)[:16]>",
  "version":           "v0.1.0",
  "host_count":        3,
  "vm_count_running":  47,
  "drivers":           ["qemu", "vz"],
  "plugins_installed": ["gitlab-runners-ha", "postgres-ha"],
  "go_version":        "go1.25.1",
  "os":                "linux",
  "arch":              "arm64",
  "uptime_seconds":    86400
}
```

Field by field :

| Field | Meaning |
|---|---|
| `anonymous_id` | 16-hex truncated sha256. Stable for the lifetime of the cluster ; survives disable+re-enable cycles. |
| `version` | `weft` binary version (CHANGELOG `[X.Y.Z]`). |
| `host_count` | Number of registered hosts. |
| `vm_count_running` | Local-host running-VM count. |
| `drivers` | Sorted set of hypervisor-kind labels in use (e.g. `apple-vz`, `qemu`). |
| `plugins_installed` | Sorted set of public plugin names. |
| `go_version` | `runtime.Version()`. |
| `os` / `arch` | `runtime.GOOS` / `runtime.GOARCH`. |
| `uptime_seconds` | Seconds since process start. |

## What we will NEVER add to this payload

Hard contract — PRs adding any must reject :

- Host / VM / project / network names.
- IP addresses (host, VM, control-plane, anything).
- Project / VM / network / volume UUIDs.
- Operator emails, OIDC subjects, ssh public keys.
- GPU / PCI BDF strings (model is fine, BDF is fingerprintable).
- Any value from `weft.hcl` (endpoint URLs, paths, secrets).
- Audit-log content.

The PII canary in `telemetry_test.go` parses the wire body and fails
the build if a UUID-shaped substring or forbidden token slips in.

## Operator workflow

```sh
# Opt in. Mints a per-cluster anonymous id on first run.
weft telemetry enable --endpoint https://aggregator.internal/telemetry

# See exactly what will be POSTed on the next tick.
weft telemetry preview

# State + endpoint + last-sent timestamp.
weft telemetry status

# Change endpoint without re-minting the cluster id.
weft telemetry enable --endpoint https://other.internal/telemetry

# Opt out. cluster_uuid is preserved on disk so a future re-enable
# keeps the same anonymous_id.
weft telemetry disable
```

`weft telemetry preview` is the audit gate : run it BEFORE enabling
to confirm the payload shape matches this doc.

## State persistence

Flag + endpoint + cluster id + install date + last-sent timestamp
live in a single JSON blob under the same registry storage as
flavors / scripts / projects (file by default, etcd when
configured). One blob per cluster.

A daemon restart is required after `enable` / `disable` for the
ticker goroutine to pick up the change. `status` and `preview`
always read live state.

## Failure semantics

- **Network error / 5xx.** Retried 3× with 1s/2s/4s backoff. Ctx
  cancellation drops the in-flight send promptly.
- **4xx.** No retry — receiver rejected the payload, hammering
  won't help.
- **All failures logged at INFO**, never ERROR — telemetry being
  down is not an operational failure.

## See also

- [`telemetry/`](../../telemetry/) — runtime sender.
- [`cmd/weft/telemetry/`](../../cmd/weft/telemetry/) — the 4 verbs.
- [`cmd/weft/main.go`](../../cmd/weft/main.go) — bootstrap ticker.
