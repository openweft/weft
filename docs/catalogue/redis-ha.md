# `redis-ha`

Three Redis members in the HA 3-DC layout. Each VM runs **both** a
Redis server and a Sentinel sidecar, so the Sentinel quorum stays
co-located with the data plane.

When you want **a cache or session store that survives a DC outage**
without operating Redis Cluster.

## What it does

- Creates a dedicated `redis` network (`10.51.0.0/24`, NAT).
- Creates a `redis-cache` security group: 6379/tcp + 26379/tcp
  ingress from tenant networks, replication / Sentinel gossip mesh
  between replicas, 53/udp DNS egress.
- Creates **three** micro-VMs (2 vCPU, 6 GiB RAM, 10 GiB root + an
  8 GiB AOF `aof` volume mounted at `/data`) with hard anti-affinity.
- Sentinel watches Redis ; on primary failure 2 of 3 Sentinels
  agree (`sentinel_quorum = 2`), elect a new primary, replicas
  re-attach.

## Inputs

| Input              | Required | Secret | Default          | Notes                                              |
|--------------------|----------|--------|------------------|----------------------------------------------------|
| `image`            | no       | no     | `redis:7-alpine` | Drop your hardened build here (Bitnami, mirror)    |
| `password`         | yes      | yes    | —                | Redis AUTH + Sentinel-to-primary auth              |
| `maxmemory_gib`    | no       | no     | `4`              | `maxmemory` ceiling per replica                    |
| `maxmemory_policy` | no       | no     | `allkeys-lru`    | `noeviction` for cache-as-store                    |
| `sentinel_quorum`  | no       | no     | `2`              | Sentinels needed to agree on failover              |

## Operator pre-flight

1. **Pick the password.** Redis 7 still uses single-secret AUTH —
   no per-client ACLs are set up by this plugin (add them by hand
   if you need them).

2. **Size the memory ceiling.** `maxmemory_gib` should be at most
   ~75% of the VM's `mem_mb` to leave room for OS, Sentinel, and
   Redis fork-on-RDB overhead.

3. **Install.**

   ```
   weft plugin install redis-ha \
     --project data \
     --input password=$REDIS_PASS \
     --input maxmemory_gib=4 \
     --input maxmemory_policy=allkeys-lru
   ```

## Verify

```
# Ask any Sentinel who the primary is.
redis-cli -h redis-ha-<short>-redis-0.weft -p 26379 \
  SENTINEL get-master-addr-by-name mymaster
# → 10.51.0.X 6379

# Connect to the primary, write + read.
redis-cli -h 10.51.0.X -p 6379 -a $REDIS_PASS SET hello world
redis-cli -h 10.51.0.X -p 6379 -a $REDIS_PASS GET hello

# Trip a failover.
redis-cli -h 10.51.0.X -p 26379 SENTINEL failover mymaster
```

## What's NOT included

- **Redis Cluster** (shard hash-slots): this is the Sentinel
  (master/replica) topology. One write primary at a time. If you
  need horizontal write scaling, deploy Redis Cluster separately —
  the Sentinel + Cluster modes are mutually exclusive.
- **Per-client ACLs**: single AUTH password only. Add
  `ACL SETUSER ...` by hand after install.
- **TLS**: 6379 is plaintext. Add `tls-port` config + cert volume
  if your tenants don't share a trusted overlay network.
- **Backups**: AOF persists writes to the volume, but there's no
  cron'd RDB snapshot to S3. Add one in a sidecar if you need
  point-in-time recovery beyond the last AOF rewrite.
