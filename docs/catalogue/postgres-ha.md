# `postgres-ha`

Three Patroni-managed PostgreSQL members in the HA 3-DC layout —
one primary, two streaming replicas, automatic failover.

When you want **a relational database that survives a DC outage**
and you'd rather not run a separate operator.

## What it does

- Creates a dedicated `postgres` network (`10.50.0.0/24`, NAT).
- Creates a `postgres-db` security group: 5432/tcp ingress from
  tenant networks, 8008/tcp Patroni REST gossip between replicas,
  53/udp DNS egress.
- Creates **three** micro-VMs (4 vCPU, 8 GiB RAM, 20 GiB root + a
  50 GiB persistent `data` volume mounted at `/var/lib/postgresql`)
  with hard anti-affinity (`az = "different"`, `host = "different"`).
- Patroni elects the leader at boot and republishes the topology via
  the cluster's embedded etcd (no separate DCS).

## Inputs

| Input                  | Required | Secret | Default                          | Notes                                                 |
|------------------------|----------|--------|----------------------------------|-------------------------------------------------------|
| `image`                | no       | no     | `quay.io/patroni/patroni:3.3.2`  | `ghcr.io/openweft/postgres-ha:v0.1.0` not yet published |
| `superuser_password`   | yes      | yes    | —                                | Postgres `postgres` user                              |
| `replication_password` | yes      | yes    | —                                | Used by replicas to stream WAL                        |
| `database_name`        | no       | no     | `app`                            | Initial DB created on bootstrap                       |
| `data_volume_gib`      | no       | no     | `50`                             | Per-replica persistent volume                         |
| `synchronous_commit`   | no       | no     | `on`                             | `on` waits for replica ack ; `off` is async           |
| `tenant_network_cidrs` | no       | no     | `10.0.0.0/8`                     | Restrict 5432/tcp ingress reach                       |

## Operator pre-flight

1. **Pick passwords** for the superuser and the replication user.
   Stash them in your secret manager — the plugin records them in
   weft's secret store, never plaintext HCL.

2. **Size the project quota**: 3 × 4 vCPU + 3 × 8 GiB RAM + 3 × 50
   GiB persistent storage.

3. **Install.**

   ```
   weft plugin install postgres-ha \
     --project data \
     --input superuser_password=$PG_SU \
     --input replication_password=$PG_REPL \
     --input database_name=app
   ```

## Verify

```
# Get the current primary's address from Patroni.
weft plugin status postgres-ha
psql "host=postgres-ha-<short>-postgres-0.weft user=postgres dbname=app" \
  -c "SELECT pg_is_in_recovery();"
# Expect: f  (this is the primary)

# Force a failover and watch a replica take over.
curl -X POST http://postgres-ha-<short>-postgres-0.weft:8008/failover
```

## What's NOT included

- **Logical replication / CDC**: no `pgoutput` publication is set up.
  Add `wal_level = logical` and create publications by hand or via a
  follow-up plugin.
- **pgbouncer / connection pooling**: clients hit Postgres directly.
  Add pgbouncer in its own VM if you have >a few hundred clients.
- **Read-replica routing**: clients have to know which member is the
  primary. Patroni's REST API on 8008 reports it ; wire that into
  your app or a sidecar HAProxy.
- **Backups**: no `pgbackrest` / `barman` ; the persistent volume is
  the only line of defence. Plan to add a backup target (S3 (versitygw-ha))
  with a sidecar in v2.
