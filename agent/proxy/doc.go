// Package proxy embeds a Caddy reverse-proxy inside weft-agent.
//
// # Why supervised subprocess rather than library
//
// Caddy is pure-Go CGO=0 and can be imported as a library (caddy.Run /
// caddy.Load). We deliberately do NOT take that route. Reasons:
//
//   - Vendor weight. Importing caddy/v2 pulls in ~30+ transitive modules:
//     certmagic, acmez, libdns, quic-go, zap, smallstep/certificates, and
//     their tails. weft's vendor tree would roughly double. The agent
//     binary would gain ~25–30 MB of compiled code for a feature most
//     deployments don't activate at all.
//
//   - Crash isolation. A panic in Caddy's TLS handshake path or in an
//     ACME challenge handler shouldn't take down weft-agent (which owns
//     the mesh, microVM lifecycle, and host registration). Subprocess
//     supervision gives us natural fault containment.
//
//   - Operational consistency. weft-agent already supervises subprocesses:
//     weft-driver-vz / weft-driver-qemu via go-plugin. Adding a Caddy
//     child fits that pattern; the agent's "lifecycle child" muscle is
//     already exercised.
//
//   - Caddy's design favours it. Caddy ships an admin API (`:2019` by
//     default) explicitly for the "tiny supervisor pokes the running
//     daemon" architecture. The whole `caddy reload` command is a thin
//     POST to that endpoint. We're using Caddy the way it expects to be
//     used.
//
// # What weft-agent owns
//
//   - Launching `caddy run --config -` (stdin) with a bootstrap config
//     that opens the admin endpoint on a unix socket (no network exposure
//     of the admin API ever).
//   - Watching the source of truth — etcd `/weft/proxy/routes/<host>` —
//     and re-rendering a Caddy JSON config every time it changes.
//   - POSTing the rendered JSON to the admin socket
//     (`/load`) on every change. Caddy applies it atomically; failures
//     return a structured error which we surface as a weft-agent log line.
//   - Tearing the subprocess down on ctx cancellation.
//
// # What Caddy owns
//
//   - ACME (HTTP-01 / DNS-01 / ALPN-01) — auto-HTTPS for every route.
//   - Certificate storage. Default for now is Caddy's filesystem default
//     (`$XDG_DATA_HOME/caddy`, isolated per-agent via the StateDir env
//     setup in supervisor.go). The next milestone is the etcd storage
//     adapter (`caddy-storage-etcd`) so multiple weft-agent hosts share
//     issued certs without re-minting them per-host. The code TODO
//     marker sits next to the XDG_DATA_HOME env in supervisor.go.
//   - Serving requests, including HTTP/2 + HTTP/3 (Caddy handles both
//     out of the box; we don't think about it).
//
// # Source of truth
//
// `/weft/proxy/routes/<host>` in etcd carries a JSON array of `Route`
// values (see route.go). weft-cluster's HCL gains a `route {…}` block in
// a follow-up commit; `weft up` translates HCL → etcd writes. The agent's
// proxy.Supervisor is purely a watcher + applier, not the source.
//
// Cross-cutting decision: see project memory
// `project_reverse_proxy_caddy.md` — Caddy only, no Envoy even as plugin,
// proxy lives inside weft-agent rather than as a separate binary.

package proxy
