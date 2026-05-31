# Reverse-proxy plane (`weft agent --proxy`)

The proxy plane is an opt-in subsystem that runs alongside the weft
agent on a host. When enabled it supervises a Caddy subprocess
(`agent/proxy.Supervisor`) and streams route updates from etcd
(`agent/proxy.Watcher`) into Caddy's admin API. Off by default —
operators that don't need L7 ingress keep the single-process daemon.

See [[project_reverse_proxy_caddy]] for the design rationale (Caddy
embedded in `weft-agent`, supervised subprocess, no Envoy, no separate
binary). The lifecycle helper is `bootProxy` in `cmd/weft/proxy.go`.

## Enabling on a host

```
weft agent --proxy --proxy-caddy-binary=/usr/local/bin/weft-proxy
```

Flags:

- `--proxy` (bool, default `false`)
  Enables the proxy plane. All-in-one mode only — the `--client`
  per-host runtime reaches etcd through the control-plane gRPC bridge,
  not a local handle, so the flag is logged-and-ignored under
  `--client` (etcd-over-gRPC shim is a follow-up).

- `--proxy-state-dir` (string, default empty)
  Directory for the Caddy admin socket + cert storage. Empty falls
  back to `proxy.Options`' default
  (`$XDG_RUNTIME_DIR/weft-agent-proxy`).

- `--proxy-caddy-binary` (string, default `caddy`)
  The Caddy executable. The default expects `caddy` on `PATH` —
  workable for dev, but production deploys should point at the
  `weft-proxy` artefact published by
  [`openweft/weft-proxy`](https://github.com/openweft/weft-proxy)
  (xcaddy-built with the `etcd-storage` module so multi-host certificate
  sharing works).

- `--proxy-key-prefix` (string, default empty)
  Override the etcd key prefix the watcher streams from. Empty falls
  back to `proxy.Watcher`'s default (`/weft/proxy/routes`). Operators
  with a multi-cluster shared etcd can namespace differently here.

## Lifecycle

`bootProxy` is called from `cmd/weft/main.go` after `buildStorageFactory`
returns, against a top-level `signalContext` (SIGINT / SIGTERM). The
deferred closers unwind LIFO :

1. SIGINT → ctx cancelled → main `run()` returns.
2. `proxyCloser()` — watcher's etcd-watch goroutine cancels, Caddy
   subprocess receives SIGTERM, `Supervisor.Close()` returns.
3. `sf.close()` — etcd client closed.

Without `--proxy` the call site is skipped entirely ; the agent still
gets a `signalContext`-driven `<-ctx.Done()` (was `select {}`) so a
SIGINT exits cleanly instead of being ignored.

## File backend (`--storage-backend=file`)

The proxy plane needs a `*clientv3.Client` to watch for route updates.
On the file storage backend that handle is nil, and `bootProxy`
degrades to "supervisor-only" mode : Caddy starts with an empty route
table and never receives updates. Useful for smoke-testing the Caddy
lifecycle, not for production. Use `--storage-backend=etcd` (or
`embed-etcd` in single-host dev) to get the full Watcher path.
