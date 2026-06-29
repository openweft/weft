# Redis HA — three Redis members + Sentinel sidecar per replica for
# automatic primary election. Sentinel runs in the same microVM as
# Redis (no separate VM tier) so a DC outage takes both processes out
# atomically and there's no orphan Sentinel quorum to worry about.
#
# This is the Sentinel (master/replica) topology, NOT Redis Cluster
# (shard hash-slots). One write primary at a time, two read replicas,
# automatic failover. See docs for when to swap to Cluster mode.
#
# Image : upstream `redis:7-alpine` by default — call out in the
# operator UI that ops with a hardened build (Bitnami, GHCR mirror)
# can swap the `image` input.
#
# Operator pre-flight (see docs/catalogue/redis-ha.md):
#   1. Pick a password (Redis auth) and the memory ceiling.
#   2. weft plugin install redis-ha \
#        --project data \
#        --input password=$REDIS_PASS \
#        --input maxmemory_gib=4

plugin "redis-ha" {
  version     = "v1"
  kind        = "cache"
  description = "Three-node Redis with Sentinel sidecars for automatic failover, one per DC"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "redis:7-alpine"
    help    = "OCI image for Redis + Sentinel. Hardened forks (Bitnami, your own GHCR mirror) drop in here."
  }

  input "password" {
    type     = "string"
    required = true
    secret   = true
    help     = "Redis AUTH password — also used by Sentinel to talk to the primary."
  }

  input "maxmemory_gib" {
    type    = "int"
    default = "4"
    help    = "Per-replica memory ceiling (GiB) fed to `maxmemory`. Match VM mem_mb minus ~25% for OS + Sentinel."
  }

  input "maxmemory_policy" {
    type    = "string"
    default = "allkeys-lru"
    help    = "Eviction policy when maxmemory is hit. `noeviction` for cache-as-store ; `allkeys-lru` for cache-as-cache."
  }

  input "sentinel_quorum" {
    type    = "int"
    default = "2"
    help    = "Number of Sentinels that must agree the primary is down before failover. With 3 replicas, 2 = majority."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "redis" {
    cidr = "10.51.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "redis-cache" {
    description = "Redis + Sentinel — 6379/tcp + 26379/tcp from tenants, gossip between replicas."
    networks    = ["redis"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 6379
      port_max    = 6379
      remote_cidr = "10.0.0.0/8"
      description = "Redis wire protocol from tenant networks."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 26379
      port_max    = 26379
      remote_cidr = "10.0.0.0/8"
      description = "Sentinel discovery — clients hit Sentinel for the current primary."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 6379
      port_max    = 6379
      remote_cidr = "10.51.0.0/24"
      description = "Inter-replica primary→replica streaming."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 26379
      port_max    = 26379
      remote_cidr = "10.51.0.0/24"
      description = "Inter-Sentinel quorum gossip."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 6379
      port_max    = 6379
      remote_cidr = "10.51.0.0/24"
      description = "Outbound replication."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 26379
      port_max    = 26379
      remote_cidr = "10.51.0.0/24"
      description = "Outbound Sentinel gossip."
    }

    rule "egress" {
      protocol    = "udp"
      port_min    = 53
      port_max    = 53
      remote_cidr = "0.0.0.0/0"
      description = "DNS."
    }
  }

  # -----------------------------------------------------------------
  # VMs
  # -----------------------------------------------------------------

  vm "redis" {
    image    = "redis:7-alpine"
    runtime  = "microvm"
    replicas = 3
    cpu      = 2
    mem_mb   = 6144
    disk_gb  = 10
    network  = "redis"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "password"         { env_name = "REDIS_PASSWORD" }
    env_from "maxmemory_gib"    { env_name = "REDIS_MAXMEMORY_GIB" }
    env_from "maxmemory_policy" { env_name = "REDIS_MAXMEMORY_POLICY" }
    env_from "sentinel_quorum"  { env_name = "REDIS_SENTINEL_QUORUM" }

    # AOF persistence — sized small. Redis-as-cache can drop this.
    volume "aof" {
      size_gib = 8
      format   = "raw"
      mount    = "/data"
    }
  }
}
