# weft-slognats

Pure-Go `slog.Handler` fan-out that pipes logs to **stderr AND a NATS
subject** in one shot. The bridge weft-doctor needs to receive the
logs it classifies — without it, the diagnosis pipeline is empty.

## Why

Every openweft component (weft-agent, weft-microvm-agent, drivers,
HA agents, runners) logs structured records via Go's `log/slog`. Until
now they all wrote to stderr only. weft-doctor subscribes to NATS and
expects JSON-encoded slog records there. This package is the one-line
swap that makes the pipeline complete : same stderr output as before,
plus WARN+ERROR records published on a per-component NATS subject.

## Wiring (one line in main.go)

```go
import "github.com/openweft/weft-slognats"

natsConn, _ := nats.Connect("nats://nats.weft.svc:4222")

log := slog.New(weftslognats.NewHandler(weftslognats.Options{
    Base:    slog.NewJSONHandler(os.Stderr, nil),
    Conn:    weftslognats.FromConn(natsConn),
    Subject: "weft.agent." + hostUUID + ".log",
}))
slog.SetDefault(log)
```

That's it. Every `slog.Info(...)`, `slog.Warn(...)`, `slog.Error(...)`
in the codebase keeps working ; WARN+ERROR records additionally land
on the NATS subject as JSON-encoded slog records — the exact format
`weft-doctor/ingest` expects.

## Subject convention

```
weft.<component>.<host_or_vm_id>.log
```

Examples :

| Component | Subject |
|---|---|
| weft-agent | `weft.agent.dc1-r1-h1.log` |
| weft-microvm-agent | `weft.microvm-agent.<vm_uuid>.log` |
| weft-driver-qemu | `weft.driver.qemu.dc1-r1-h1.log` |
| weft-ha-postgresql | `weft.ha.postgres.dc1-r1-h1.log` |

weft-doctor subscribes via `weft.>` (everything) or finer wildcards
to scope its analysis.

## Degraded mode

`Conn: nil` is supported : the handler degrades to base-only (stderr
only), no panic. Useful for :

- Local dev / tests (no NATS reachable)
- Initial bring-up when NATS isn't running yet
- A NATS outage where you still want stderr logging

When `Conn != nil` but the publish fails at runtime, the error is
surfaced via `Options.OnPublishError` (defaults to no-op) but never
bubbles back to the caller — logging must never fail the caller.

## Configuration

| Option | Default | Notes |
|---|---|---|
| `Base` | — | Required. nil panics at startup. |
| `Conn` | `nil` | nil = base-only (graceful degrade). |
| `Subject` | — | Required when `Conn != nil`. |
| `MinLevel` | `slog.LevelWarn` | Floor for NATS publish (stderr always gets everything Base allows). |
| `OnPublishError` | no-op | Callback on Publish failure. |

## Payload schema

The NATS payload is byte-for-byte the same as what `slog.JSONHandler`
emits to stderr (minus the trailing newline) :

```json
{"time":"2026-06-08T19:42:01Z","level":"ERROR","msg":"VM start failed","vm_uuid":"abc","retry":3}
```

This means weft-doctor's `ingest.parseSlog` has ONE format to handle
across all components.

## Build matrix

Pure-Go, CGO=0. Tested cross-build : `linux + darwin + openbsd +
freebsd + netbsd × amd64 + arm64` (10 targets).

## License

BSD 3-Clause — see LICENSE.
