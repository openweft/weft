# weft/infra

Declarative plans for the platform's own infrastructure services
running as **micro-VMs** on weft. Each subdirectory describes one
service (etcd, zot, dex, …) ; the directory layout is intentionally
parallel so adding a fourth service is mechanical.

```text
infra/
├── README.md          (this file)
├── etcd/
│   ├── plan.hcl       declarative spec: OCI ref, RAM, CPU, volumes, net
│   └── README.md      service-specific notes
├── zot/
│   ├── plan.hcl
│   └── README.md
└── dex/
    ├── plan.hcl
    └── README.md
```

`weft infra deploy <service>` reads the HCL, validates the
pre-pulled OCI rootfs is on disk (operator runs `weft-microvm pull` first
until weft grows its own OCI client), calls `RegisterMicroVM`, then
`StartVM`. Same management plane as user workloads — `weft
start/stop/status` works identically.

`weft infra bootstrap` does the same thing for every plan at once :
discovers everything under `infra/`, topologically sorts by
`depends_on` (lexical tiebreak for determinism), then deploys each
in order through a shared Adapter. Use `--services foo,bar` to
narrow the set (their `depends_on` must resolve inside the
narrowed set too, otherwise bootstrap errors). No health checks
yet — a service is "deployed" once `StartVM` returns ; gate
readiness manually with `weft infra deploy` in sequence until the
HealthBlk poller lands.

`weft infra validate [--services foo,bar]` is the dry-run lint:
load every plan, run the same parse + dependency + cycle checks
bootstrap would run, and print the deploy order it would use.
No VM is registered. Use it before editing a plan lands in main
to catch shape mistakes early (unknown HCL attributes, missing
`depends_on` targets, dependency cycles, plan-label vs
directory-name mismatches) :

```sh
$ weft infra validate
# 4 plan(s) validated, deploy order:
 1. etcd  (depends_on: —)
 2. dex   (depends_on: etcd)
 3. nats  (depends_on: dex)
 4. zot   (depends_on: etcd,dex)
```

Exits non-zero on the first error so a CI gate can fail-fast.

`weft infra status` lists every plan and the live state of its VM
in tab-separated form (`SERVICE STATE PROJECT IMAGE`). Useful
right after `bootstrap` to verify everything came up :

```sh
weft infra status
# SERVICE STATE          PROJECT IMAGE
# dex     running        infra   ghcr.io/dexidp/dex:v2.40.0
# etcd    running        infra   quay.io/coreos/etcd:v3.6.0
# nats    not-registered infra   docker.io/nats:2.11-alpine
# zot     running        infra   ghcr.io/project-zot/zot:v2.1.0
```

`STATE` is one of `not-registered` (no VM dir yet), `stopped`
(dir exists but `vm.pid` absent / stale), or `running`.

## Config-file materialisation

A plan with a `config_file { path = ..., template = ... }` block
gets its template rendered at deploy time and written to
`<stateDir>/infra-config/<service>/<basename>` (mode 0600). The
deployer then appends a virtio-fs share at tag `cfg` pointing at
that directory. The plan's `cmdline` (e.g. `... weft.config=virtiofs:cfg`)
tells weft-microvm-init where to find it ; the OCI image's entrypoint or
weft-microvm-init pre-exec step copies the file into the right in-guest
path.

Token substitution runs at deploy time on the in-scope subset
(`$REPLICA`, `$DC`, `$PRIVATE_IP`, `$PEERS`, `$PEER_DC`) with a
word-boundary regex so `$DC` doesn't bleed into `$DCFOO`. The
single-replica deployer uses : `$REPLICA = 1`, `$DC = "dc1"`,
`$PRIVATE_IP = network.static_ip[0]` (empty if none), `$PEERS =
""`, `$PEER_DC = ""`.

Operator-side tokens (`$BASE_DOMAIN`, `$ADMIN_BCRYPT_HASH`,
`$WEFT_CLIENT_SECRET`, …) are intentionally **not** substituted —
they pass through verbatim so downstream envsubst / CI templating
handles them.

When per-DC replica fan-out lands, the deployer will build one
`TemplateContext` per replica with the right index / IP / peer
list ; the rendering machinery stays the same.

## Placement (multi-replica HA)

A plan declares its HA intent with a `placement { ... }` block.
The deployer translates it into a `weft.GroupScheduleRequest` and
places `count` replicas across the cluster honouring the three
proximity dimensions independently :

```hcl
placement {
  count = 3                # how many replicas
  az    = "different"      # one per AZ
  rack  = "different"      # one per rack (within each AZ)
  host  = "different"      # one per hypervisor
}
```

Proximity values are `"same"`, `"different"`, or omitted (no
constraint). `count` defaults to 1 ; omit the whole block for a
single-replica service.

The placement hierarchy is `AZ ⊃ Rack ⊃ Host` — Rack carves a
sub-AZ failure domain (ToR switch, PDU) for clusters that have
multiple racks per AZ. Single-rack dev clusters leave `Rack`
unset on every host ; `rack = "different"` then fails-safe
(can't prove distinctness with missing data) so the operator
notices the gap.

Replica fan-out is live in the deployer: `deployPlan` loops over
`p.ReplicaCount()` replicas, calling `deployReplica` for each.
Per-replica artefacts :

- VM name : `infra-<service>-dc<i>` for count > 1 (count = 1
  keeps the legacy `infra-<service>` shape for backward compat).
- Config-file scratch dir : `<stateDir>/infra-config/<service>-dc<i>/`
  with the template rendered through `BuildReplicaContext(p, i)`
  so `$REPLICA / $DC / $PRIVATE_IP / $PEERS / $PEER_DC` reflect
  the replica's position in the group.
- `weft infra status` reports each replica as its own row :

```sh
weft infra status
# SERVICE         STATE          PROJECT IMAGE
# infra-nats-dc1  running        infra   docker.io/nats:2.11-alpine
# infra-nats-dc2  running        infra   docker.io/nats:2.11-alpine
# infra-nats-dc3  running        infra   docker.io/nats:2.11-alpine
```

Scheduler integration (per-replica Host pick honouring the
placement rule) is the next slice — today the deployer runs
locally on the host weft is on, so the scheduler primitive
`weft.ScheduleGroup` isn't yet on the critical path.

## Health probes

Plans declare a readiness probe with the `health { ... }` block.
Today only `type = "http"` is implemented ; the deployer polls
`cmd` with HTTP GETs every `period` until a 2xx response arrives
(or `--health-timeout` elapses).

The URL is **host-side** : the plan author writes the address the
deployer should reach. Because the host cannot dial `127.0.0.1` /
`localhost` and land in the guest, use the literal token `$VM_IP` —
the deployer substitutes it with the guest's network IP (discovered
via `Adapter.IP(vmName)`) before each probe :

```hcl
health {
  type   = "http"
  cmd    = "http://$VM_IP:8222/healthz"
  period = "5s"
}
```

Enable the probe with `--wait-health` on `weft infra deploy` or
`weft infra bootstrap` — without that flag the deployer
considers a service "deployed" as soon as `StartVM` returns
(same as before, no regression). With `--wait-health` the
deployer waits in two phases :

1. Poll `Adapter.IP(vmName)` until the guest has a host-side
   address (half the budget).
2. Substitute `$VM_IP` into `health.cmd` and poll the URL until
   2xx (the other half).

`type = "exec"` (run a command inside the guest) is recognised
but not yet wired — surfaces a clear error so the operator
notices instead of the bootstrap silently skipping the probe.

## Plan schema notes

Persistent volumes are declared as repeated `volume { ... }`
blocks rather than a `volumes = [ ... ]` list. HCL's gohcl
attribute decoder needs cty tags on the struct to decode a
list-of-object literal, while the block form just works :

```hcl
volume {
  mount    = "/var/lib/<service>"
  uuid     = "<service>-data-dc1"
  size_gib = 32
}
volume {
  mount    = "/var/lib/<service>"
  uuid     = "<service>-data-dc2"
  size_gib = 32
}
```

The `health { ... }` block uses `cmd` for both the exec command
and the URL to poll — the field is overloaded by `type`. Picking
`url` for HTTP probes won't decode (HCL's strict mode rejects
unknown attributes).

## Bootstrap order

Each step depends on the previous one. Step 1 is the only
operator-side action; everything after is one CLI invocation.

```text
1. weft starts in FILE storage mode
     │
     │ reads ~/.config/weft/projects.hcl from disk
     │ no production etcd yet
     ▼
2. weft infra bootstrap         (everything below in topo order)
     │  — or, equivalently, the explicit chain:
     │
     ├── weft infra deploy etcd     (3 VMs, one per AZ, HA cluster)
     │   private control-plane subnet
     │   each VM mounts a dedicated qcow2 for /var/lib/etcd
     │
     ├── weft infra deploy coredns  (3 VMs, one per AZ, HA cluster)
     │   tenant-services subnet, IPs .53/.54/.55
     │   weft.internal zone served from etcd ; recursive for the rest
     │   so every later service can reference names instead of IPs
     │
     ├── weft infra deploy dex      (1 VM per AZ, HA via LB front)
     │   statics-bootstrap user (admin / one-time password)
     │   etcd storage backend (co-tenants the just-deployed cluster)
     │   federation to upstream IdP can wait until after self-promote
     │
     ├── weft infra deploy nats     (3 VMs, one per AZ, JetStream cluster)
     │   tenant-services subnet
     │   per-project subject auth via dex-issued NKey JWTs
     │
     └── weft infra deploy zot      (1 VM per AZ, sync mirror between)
         bearer.realm pointing at dex.weft.internal
         local-fs storage (dev) or S3-compat (prod)
     │
     ▼
3. weft self-promote --storage=etcd
     │
     │ migrates projects/users/networks/volumes/... from local
     │ FILE storage into EtcdStorage keys under
     │ /weft/<env>/<registry>
     │ FILE remains as a degraded-mode write-ahead snapshot
     ▼
4. (optional) weft infra federate-dex --upstream-ldap=ldap://…
     │
     │ once dex is up + zot accepts its tokens, the upstream IdP
     │ federation is a config-only change
     ▼
SELF-HOSTED: weft now depends on infra it brought up itself.
```

## Why plans are HCL and not YAML

Per [[hcl-over-json]] : weft's registries, configs, and infra plans
default to HCL. Comments allowed, expressive enough for the
templating + reference shapes (`volume = volume.etcd-data-dc1.uuid`),
and human-pokable. The third-party YAML configs each service
expects (etcd.conf.yaml, dex's config.yaml, zot-config.json) are
emitted by `weft infra deploy` from the plan — operators don't
maintain a duplicate YAML.

## Why micro-VMs over containers

- **Hardware isolation** : KVM / HVF boundary, identical to user
  workloads. A bug in dex doesn't expose the same kernel surface
  to etcd.
- **One management plane** : `weft start/stop/status` works for
  etcd-dc1 the same way it works for `team-alpha/db-prod`. No
  systemd-units for infra and weft-VMs for users.
- **Per-VM kernel** : the etcd VM can run a kernel tuned for
  low-jitter (`CONFIG_PREEMPT_NONE=y`,
  `CONFIG_NUMA_BALANCING` off), distinct from the user-VM kernel
  choices, without coupling the two.
- **OCI as the unit of distribution** : pulling `quay.io/coreos/
  etcd:v3.6.0` is the same code path that pulls any other OCI
  artifact. No special "bare-metal install" recipe.

The cost is the per-VM overhead (a tiny Linux kernel + weft-microvm-init
per service), which at 5 MiB RAM and ~250 ms cold boot is in the
noise compared to the service's own footprint.

## Wiring shape (in weft codebase)

The HCL parser lives next to the [[weft-uuid-keyed-resources]]
registries (`pkg/openweft/weft/infra.go`, to be written). Each
plan deserialises into a Go struct, which the deployer translates
into a `RegisterMicroVMRequest` + `StartVMRequest` pair against
weft's own gRPC API. The deployer runs in a side-process (`weft
infra deploy`) so it can be invoked during bootstrap before the
main weft daemon's REST endpoint is reachable.

After self-promote, the deployer becomes "just another weft
client" — its state moves to etcd, and HA failover means any DC's
weft can re-deploy a misbehaving infra VM transparently.
