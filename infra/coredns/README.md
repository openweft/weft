# coredns micro-VM

Cluster-internal DNS resolver. Serves the `weft.internal` zone
authoritatively (records sourced from the same etcd cluster
that backs the rest of the control plane) and forwards
everything else to operator-configured upstreams.

## Why CoreDNS first (after etcd)

Every infra service deployed after etcd benefits from name
resolution :

- `dex` resolves the upstream IdP host (`ldap.example.com`),
  the etcd endpoints (`etcd-dc1.weft.internal`), and exposes
  itself at `dex.weft.internal`.
- `zot` resolves `dex.weft.internal` for the bearer-realm,
  resolves peer zots for sync mirroring.
- `nats` cluster peers find each other at
  `nats-dc{1,2,3}.weft.internal`.

Without CoreDNS, every plan would have to hard-code IPs.
CoreDNS lands right after etcd in the bootstrap topo-sort
([[infra-in-micro-vms]]) so the rest of the chain can switch
to names cleanly.

## Why etcd-backed zone data

The `etcd` plugin in CoreDNS reads zone records from etcd keys
under a configurable prefix (`/weft/dns/` in our Corefile).
That gives us :

- **Linearizable updates** — a record write on one host shows
  up on every CoreDNS replica immediately (etcd's watch).
- **Quorum HA** — the same 3-DC etcd cluster that protects
  the rest of the control plane protects the DNS zone.
- **Same backup story** — one etcd snapshot covers vzd's
  registries, dex's sessions, AND the DNS zone.

The data plane stays masterless : every CoreDNS replica
serves queries independently, only the zone-write path
touches etcd. A CoreDNS replica that loses etcd connectivity
keeps answering queries from its last-known zone (etcd
plugin caches) until the partition heals.

## Why 3 replicas, one per AZ

Anti-affinity at AZ / rack / host level (see the
`placement { ... }` block in [plan.hcl](plan.hcl)) — a single
AZ / rack / host failure takes exactly one CoreDNS replica
down ; the other two keep resolving queries. Tenant
workloads + infra services see one of three static IPs as
their resolver list (`10.255.3.53` / `.54` / `.55`) and try
in order.

## Zone-write API

Today : operator writes etcd keys directly via `etcdctl` or
through a small `vzc dns put` CLI (TBD). The natural follow-up
is to wire `RegisterHost` / `RegisterMicroVM` into a hook that
mints A records for each new VM under `<vm-name>.<project>.weft.internal`.

## Token validation in CoreDNS

CoreDNS doesn't need OIDC tokens itself — the etcd it consumes
runs on the private control-plane subnet, behind the same
mutual-TLS that protects vzd's reads. End-user queries arrive
on UDP/53 unauthenticated (DNS is a public protocol).

## Plan source

[plan.hcl](plan.hcl)
