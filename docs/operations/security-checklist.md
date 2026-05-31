# Security audit checklist (V1 prep)

This document is the operational gate that needs to clear before tagging
a weft cluster as "V1 production-ready". It is organised by concern area
(transport, auth, secrets, supply chain, host hardening, runtime
exposure), then closes with a short pre-flight checklist meant to be
ticked off, in order, before flipping a cluster to prod.

Items flagged **GAP** are known shortcomings — capture them in your
operational risk register before declaring V1 ; do not paper over them.

## 1. Transport security

### Inter-host gRPC

Weft daemons talk to each other over gRPC. The default transport is
SSH (via the vendored `github.com/grpc-transports/ssh` module) which
piggybacks on the host's existing SSH trust store — no separate PKI
to provision.

- **Default in 3-DC bring-up**: `grpc-transports/ssh` with the host's
  configured SSH host key + `authorized_keys`.
- **OpenPubkey hook**: `grpc-transports/ssh` v0.2.0 (vendored) ships
  with an OpenPubkey hook that lets you accept short-lived,
  OIDC-attested SSH certificates instead of long-lived static keys.
  Enable via the SSH server's `Authenticator` slot — see the
  upstream README ; off by default.
- **mTLS option**: available via `grpc-transports/tls` (not the default).
  Use this where you want PKI-rooted trust independent of SSH. Cluster
  bring-up does not provision a CA today — you bring your own.

### WireGuard mesh

Post-unification, every host runs the kernel WireGuard backend
(`vendor/github.com/grpc-transports/wireguard/device_kernel_linux.go`)
when available, with a userspace fallback (`device_kernel_other.go`)
for hosts lacking the kmod.

- **Pre-shared key**: generated per-host at bring-up. **Never share
  a PSK across hosts**.
- **Per-peer pubkeys**: gossiped via the agent's pull-reconcile path.
- **Key rotation** — **GAP**: there is no automated rotation today.
  Manual rotation procedure: stop the agent on the host, remove the
  `wg0` interface, re-run `weft up` for that host (regenerates the
  private key, re-publishes the pubkey). Document this in your runbook
  and add a calendar reminder. V1 follow-up: agent-driven rotation
  on a configurable cadence.

### HTTPS for Caddy / weft-webui

The reverse-proxy plane (`weft agent --proxy`, see
`docs/operations/proxy.md`) terminates HTTPS via Caddy.

- **ACME / Let's Encrypt**: covered out of the box. Set the email
  + domain in the Caddy HCL block and Caddy handles cert issuance
  and renewal.
- **Internal CA option** — **GAP for V1**: today, Caddy can be pointed
  at an internal CA via raw Caddyfile overrides, but weft has no
  first-class HCL knob for it. Document the override path in your
  runbook ; V1 follow-up is a dedicated `proxy.tls.internal_ca` field.

## 2. AuthN and AuthZ

### OIDC issuer

3-DC bring-up provisions Dex by default. The webui and gRPC interceptor
both consume the same OIDC issuer.

- Dev mode allows wide-open access (skipped OIDC). **This MUST be off
  in prod** — see the pre-V1 checklist below.
- Token validation happens in `auth.go`'s gRPC interceptor ; the
  resulting `*Caller` is parked on the request context.

### RBAC

The wire-level checks live in `acl.go` and the model is documented in
`docs/operations/rbac.md` (group-based, scope-aware, verb-typed):

- **Subjects**: OIDC-derived (`groups`, `email`, `sub` claims).
- **Verbs**: RPC name + implicit op (Read, CreateVM, DeleteVolume…).
- **Objects**: typed resources (`vm`, `volume`, `network`, …).
- **Scopes**: `cluster` or `project:<uuid>`.

Group claims with the `weft:` prefix are recognised by the interceptor
(`weft:admin`, `weft:project:<uuid>`). Map your IdP groups accordingly.

### Audit log

- **GAP**: RBAC decisions are **not** logged today. There is no
  per-request audit trail capturing
  `subject → verb → object → scope → allow|deny`. V1 follow-up:
  structured audit log emitted on the eventbus, archived alongside
  observability traces. Until then, document this gap and rely on
  request-level access logs from Caddy for forensic context.

## 3. Secret management

### On-disk secrets

PATs, OIDC client secrets, signing keys, etc. — all stored under
`/etc/weft/` with file mode `0600` owned by the agent's user.
The systemd unit's `ProtectHome` + `ProtectSystem=strict` keep the
directory unreadable to anything else.

### Rotation

- **GAP (manual only)**: there is no automated rotation. Rotation
  procedure today:
  1. Generate the new credential out-of-band.
  2. Update `/etc/weft/weft.hcl` (or the env-var override) on each host.
  3. `systemctl reload weft-agent` (the agent re-reads on SIGHUP).
- V1 follow-up: agent-side rotation of agent-owned secrets (WireGuard
  PSK, etcd peer auth) on a configurable cadence.

### Container registry credentials

Weft does **not** bake registry credentials into its config. The OCI
client (used by `boot.Runner` and the kernel pull) falls back to the
standard `~/.docker/config.json` for the user running the agent. This
matches the behaviour operators already have with `docker login` /
`crane auth login`.

Avoid putting long-lived registry PATs on disk: prefer the IdP-issued
short-lived OIDC tokens that ghcr.io and ecr both accept.

## 4. Container image signing

- **cosign verify path** — **GAP for V1**: weft does not verify cosign
  signatures on OCI artifacts it pulls today (kernel images,
  microvm rootfs, driver plugins). Operators who want signed images
  can run `cosign verify` locally against the ghcr.io publishes
  before configuring `microvm.kernel_ref` etc. — but weft itself has
  no built-in verification path.
- **SBOM** — **GAP**: no SBOMs are generated for weft's own releases
  or for the OCI artifacts it publishes.
- **V1 prep items**: wire `sigstore/cosign` verification into the
  imagestore pull path ; generate SBOMs in CI (cyclonedx or spdx)
  and attach to GitHub releases.

Until both are wired, **pin OCI references by digest** (not by tag) in
`cluster.hcl` — this gives you integrity (different bytes → different
digest → pull fails) even without signature verification.

## 5. Supply chain

- **Pinned versions**: `go.mod` pins exact versions, no carets, no
  `v0.0.0-…+incompatible` slop. Verify with `go list -m all`.
- **Vendored**: everything is vendored under `vendor/` ; builds are
  reproducible without network. CI builds use `-mod=vendor`.
- **Dependency review**: run `go mod why <module>` for any new direct
  dep before merging. Indirect deps should track upstream pins.
- **Renovate / Dependabot** — **recommended for V1**: not enabled today.
  Pick one and configure it against `go.mod` + `vendor/` ; weekly
  cadence with grouped PRs is a sane starting point. Note that the
  vendored `grpc-transports/*` and `openweft/*` modules are
  source-mirrors maintained in this org — exclude them from external
  bumpers and rely on the source repos' own CI.

## 6. Host hardening

The agent runs under systemd ; the unit ships in the cloud-init
template (see `docs/operations/cloud-init.md`).

- **Capabilities**: the agent needs `CAP_NET_ADMIN` only — for the
  WireGuard / overlay / proxy paths. Everything else is dropped.
- **AmbientCapabilities**: set to `CAP_NET_ADMIN` in the systemd unit
  shipped by cloud-init. Verify with `systemctl show weft-agent | grep
  -i capabilities`.
- **No privileged container shims**: weft does not invoke docker /
  containerd. Driver plugins run as the agent user, supervised by
  go-plugin over a private socket.
- **Filesystem**: `ProtectSystem=strict`, `ProtectHome=true`,
  `NoNewPrivileges=true`. The agent writes only under `/etc/weft`,
  `/var/lib/weft`, and `/run/weft`.

## 7. Runtime exposure

### Caddy admin endpoint

By default Caddy's admin API binds to a **unix socket only**
(`/run/weft/caddy-admin.sock`). The Watcher streams route updates to
that socket from the same host — no TCP exposure.

- TCP exposure is opt-in via the HCL `proxy.caddy_admin.tcp` block.
  **If you enable it, you MUST also configure a firewall rule**
  scoping access to the operator's bastion. See
  `docs/operations/proxy.md`.

### weft-agent gRPC

The agent's gRPC endpoint is SSH-secured by default, bound to the
host's SSH listener. Dev mode allows a plain TCP listener
(`--grpc-listen=tcp://0.0.0.0:7777`) for local iteration.

- **The plain-TCP listener MUST require an explicit opt-in flag.**
  Today: the flag is required ; the daemon refuses to start with a
  plain TCP listener in `--prod` mode. Re-verify this stays true in
  release builds.

## 8. Pre-V1 checklist

Tick these off, in order, before declaring a cluster prod-ready.

- [ ] **OIDC issuer configured** — `auth.oidc.issuer` set in
      `/etc/weft/weft.hcl` ; `auth.dev_mode = false` everywhere.
- [ ] **OIDC group claim mapped** — IdP groups prefixed with `weft:`
      (e.g. `weft:admin`, `weft:project:<uuid>`). Verify with a test
      login that the resulting `*Caller` carries the expected groups.
- [ ] **WireGuard per-host keys** — every host generated its own
      PSK + private key. No shared keys across hosts. Verify with
      `wg show wg0 private-key` from two hosts and confirm they differ.
- [ ] **SSH host key + authorized_keys provisioned** — every host has
      a stable SSH host key (not regenerated on reboot) and the
      operator team's keys in `authorized_keys`.
- [ ] **etcd backup cron active** — per `docs/operations/etcd-backup.md`.
      Restore the most recent backup into a scratch cluster as part
      of the V1 gate.
- [ ] **HA failover drill executed** — per
      `docs/operations/ha-failover.md`, within the last 90 days. If
      you don't have a passing drill, you don't have HA.
- [ ] **Caddy admin endpoint not exposed on TCP without firewall** —
      if the HCL has `proxy.caddy_admin.tcp`, confirm the host firewall
      drops everything but the bastion CIDR.
- [ ] **OCI references pinned by digest** — every `cluster.hcl` ref
      (`microvm.kernel_ref`, `drivers.*.ref`, `boot.image`) uses
      `@sha256:…`, not a tag. Until cosign verify is wired, digest
      pinning is the only integrity mechanism.
- [ ] **RBAC audit gap documented** — operational risk register
      acknowledges the missing RBAC audit log. V1 follow-up tracked.
- [ ] **Secret-rotation runbook reviewed** — the manual procedure for
      PAT / OIDC client / WireGuard key rotation is current and a
      named owner is on the rotation cadence.
- [ ] **Dependency bumper configured** — Renovate or Dependabot active
      on the weft repo (excluding the vendored source-mirror modules).

When every box is checked, you have V1.
