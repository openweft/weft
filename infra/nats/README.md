# NATS micro-VM

Event bus for the platform — 3-DC NATS cluster with JetStream for
durable streams. Carries `vzd.events.>` traffic produced by every
vzd mutation and consumed by `vzc events`, `ncl events`, and
tenant workloads via the per-project subject scheme.

## Subject layout

Two consumer classes share one subject tree :

```text
vzd.events.<kind>                                 # global (operator-visible)
vzd.events.project.<uuid>.events.<kind>           # per-project mirror (tenant-visible, read-only)
vzd.events.project.<uuid>.app.<kind>              # per-project app stream (tenant publish + subscribe)
```

Tenant-safe kinds (`vm.*`, `guest.*`, `server.*`, `volume.*`,
`network.*`) are dual-published by vzd onto both the global and
per-project `events.` subjects ; sensitive kinds (`project.*`,
`user.*`, `*.member_added`) stay on the global subject only. The
`app.` sibling carries traffic produced **by** the project's
workloads — health pings, custom domain events — and never
contains platform-generated events. See
[vzd-tenant-event-access memory entry](../../../../../../.claude/projects/-Users-david-delavennat-Documents-VCS-GIT-localhost-cloud-boot/memory/vzd_tenant_event_access.md)
for the rationale and the four-phase roadmap.

## Auth phases

### Phase 1 — global subject only (✅ landed)

`NATSEventBus.Publish` writes to `vzd.events.<kind>`. No tenant
access yet ; vzc / ncl read with an OIDC token via vzd's
`WatchEvents` RPC.

### Phase 2 — per-project NKey provisioning (✅ landed)

Each project gets a NATS user-NKey, minted lazily on first
`RegisterMicroVM`. The seed is stored on `Project.NATSUserSeed`
(see `projects.go`) and materialised into each microVM at
`<vmDir>/nats/nats.nkey` (mode 0600), exposed read-only over a
virtio-fs share with tag `vzd-nats`. Tenants authenticate with
`nats.Nkey(pub, kp.Sign)`. **No server-side enforcement yet** —
the seed sits on the guest, ready for Phase 3.

### Phase 3 — server-side subject permissions (this directory)

`Adapter.RenderNATSAuthorization` (see `nats_config.go`) emits a
NATS `authorization { … }` block from the current project
registry :

```
authorization {
  default_permissions = {
    publish:   { deny: [">"] }
    subscribe: { deny: [">"] }
  }
  users = [
    { nkey: "U…<vzd-admin>", permissions: {
      publish:   { allow: ["vzd.>"] }
      subscribe: { allow: ["vzd.>"] }
    } },
    # one entry per project with a NATSUserSeed
    { nkey: "U…<project-1-pubkey>", permissions: {
      subscribe: { allow: ["vzd.events.project.<uuid-1>.events.>"] }
      publish:   { allow: ["vzd.events.project.<uuid-1>.app.>"] }
    } },
    …
  ]
}
```

The rendered block is **deterministic** : projects sort by UUID so
that a registry mutation produces a localised diff. Output is the
NATS conf format (a comment-friendly JSON superset that
nats-server reads natively) — not HashiCorp HCL, because
nats-server doesn't parse HCL.

### Auto-render

`Adapter.SetNATSAuthorizationFile(path, adminPubkey)` turns on
hooks that re-render the block after every mutation that changes
its output : seed mint inside `RegisterMicroVM`, and project
delete. The write is atomic (tmp + rename, mode 0600), and the
parent directory is created if missing.

Enable it from `vzd.hcl` with a `nats_authorization { ... }`
block :

```hcl
event_bus {
  backend = "nats"
  nats { url = "nats://nats.internal:4222" }
}

nats_authorization {
  path         = "/etc/vzd/nats-authorization.conf"
  admin_pubkey = "U..."            # vzd's own NATS NKey
}
```

Tilde-expansion works on `path`. For operator-driven setups
(no auto-write) just omit the block — the renderer stays callable
via `vzc admin nats-authz`.

### Reload story

NATS server picks up authorization changes via
`nats-server --signal reload` (which it implements natively). The
rendered file is meant to be `include`d from `nats.conf` on each
NATS micro-VM ; the reload trigger today is one of :

- Operator runs `nats-server --signal reload` after the file
  changes (visible via `inotifywait` on the include path).
- Future : vzd emits a `nats.authorization_changed` control-plane
  event ; a small in-VM sidecar watches the bus and signals
  nats-server.
- Future : `vzc admin nats reload-authz` RPC pushes the render
  through vzd to each NATS micro-VM and triggers `signal reload`
  remotely.

For Phase 3 dev / single-host the operator runs the renderer by
hand, pipes the output to a file nats-server includes, and signals
the local nats-server. The exact wiring will firm up when the NATS
micro-VM bootstrap actually moves off the cleartext fallback in
[plan.hcl](plan.hcl).

### Phase 4 — tenant publish ✅ ; JWT next

`Adapter.RenderNATSAuthorization` now emits a `publish: allow`
entry on `vzd.events.project.<uuid>.app.>` for every project — the
same NKey that subscribes to the events mirror can publish to the
sibling `app.` subject. The events tree stays read-only for
tenants (no overlap between `subscribe.allow` and `publish.allow`)
so a workload can't forge platform events back into the operator
stream even with its own creds. Tests in `nats_config_test.go`
(`TestRenderNATSAuthorization_AppNamespaceIsolated`) pin that
boundary.

Still pending in this phase :

- Migrate the static `authorization` block to a JWT / Account /
  Operator hierarchy issued by dex so user provisioning flows
  through the control plane rather than through a config reload.
- Wire vzd to auto-render the block on every project mutation and
  push it to each NATS micro-VM, then signal `nats-server --signal
  reload` remotely (today the operator runs `vzc admin nats-authz`
  and updates the file by hand).

## Plan source

[plan.hcl](plan.hcl)
