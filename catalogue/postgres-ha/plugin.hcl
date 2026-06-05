# PostgreSQL HA — three Postgres members managed by our own
# `weft-ha-postgresql` agent (Go, openweft-built) sitting next to each
# replica's Postgres instance. The agent does the HA reconcile :
#
#   - etcd-backed leader election (concurrency.NewElection, lease-bound)
#   - role API at :8008 (Caddy in weft-agent uses it as an active health
#     check ; the upstream pool routes traffic to whichever replica
#     returns 200 at /primary)
#   - VMFencer : stops a fenced primary via the weft-agent gRPC StopVM
#     RPC BEFORE the candidate promotes — never two writers
#   - synchronous_standby_names recomputed every tick so RPO 0 across DCs
#     is automatic when peers come and go
#
# This replaces the previous Patroni-based deployment (Patroni was the
# placeholder for v0.1 ; v0.2 ships our native operator). The structural
# advantage : our agent owns the substrate, so it can PROVE a fenced
# primary is dead rather than rely on a cooperative watchdog inside the
# guest — see weft-ha-postgresql/internal/fencing.
#
# Image : `ghcr.io/openweft/postgres-ha:v0.2.0` — pure Go agent + stock
# Postgres in a single rootfs. Entrypoint runs Postgres + the agent ; the
# agent's exit signal also drains Postgres.
#
# Operator pre-flight (see docs/catalogue/postgres-ha.md):
#   1. Pick a superuser password + replication password and stash them
#      in a secret manager — the plugin records them in the secret
#      store, never on disk in plaintext.
#   2. weft plugin install postgres-ha \
#        --project data \
#        --input superuser_password=$PG_SU \
#        --input replication_password=$PG_REPL \
#        --input database_name=app

plugin "postgres-ha" {
  version     = "v1"
  kind        = "database"
  description = "Three-member PostgreSQL cluster managed by weft-ha-postgresql (etcd DCS + VMFencer + Caddy routing). Replaces the Patroni layout."
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "ghcr.io/openweft/postgres-ha:v0.2.0"
    help    = "Postgres + weft-ha-postgresql agent OCI image. The Go agent + stock Postgres in one rootfs."
  }

  input "superuser_password" {
    type     = "string"
    required = true
    secret   = true
    help     = "Password for the `postgres` superuser. Cluster secret store, never plain HCL."
  }

  input "replication_password" {
    type     = "string"
    required = true
    secret   = true
    help     = "Password used by replicas to stream WAL from the primary."
  }

  input "database_name" {
    type    = "string"
    default = "app"
    help    = "Initial database created on bootstrap. Owner = `postgres` ; create app users afterward."
  }

  input "data_volume_gib" {
    type    = "int"
    default = "50"
    help    = "Per-replica persistent volume for /var/lib/postgresql. Resize requires a rolling restart."
  }

  input "synchronous_commit" {
    type    = "string"
    default = "on"
    help    = "Postgres `synchronous_commit` setting. `on` = wait for one off-DC replica ack (RPO 0 on DC outage). `off` = async (lower latency, possible data loss)."
  }

  input "tenant_network_cidrs" {
    type    = "string"
    default = "10.0.0.0/8"
    help    = "Comma-free CIDR (single string) of tenant networks allowed to reach 5432/tcp. Default opens the RFC1918 10/8 superblock — narrow it for production."
  }

  input "fence_timeout_sec" {
    type    = "int"
    default = "30"
    help    = "How long the agent waits for a confirmed-stopped state during fencing. Timeout MUST block promotion ; never invent 'probably stopped'."
  }

  input "etcd_session_ttl_sec" {
    type    = "int"
    default = "15"
    help    = "etcd lease TTL. Lower bound on failover latency : a fenced primary's lease expires after this many seconds."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "postgres" {
    cidr = "10.50.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "postgres-db" {
    description = "Postgres replicas — 5432/tcp from tenants, 5432/tcp inter-replica for streaming replication, 8008/tcp role API for Caddy health checks."
    networks    = ["postgres"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 5432
      port_max    = 5432
      remote_cidr = "10.0.0.0/8"
      description = "PostgreSQL wire protocol from tenant networks (narrow via tenant_network_cidrs input)."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 5432
      port_max    = 5432
      remote_cidr = "10.50.0.0/24"
      description = "Replica → primary streaming replication."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8008
      port_max    = 8008
      remote_cidr = "10.0.0.0/8"
      description = "Role API (/primary, /replica, /health) for the L7 Caddy in weft-agent's active health check."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 5432
      port_max    = 5432
      remote_cidr = "10.50.0.0/24"
      description = "Outbound WAL streaming between replicas."
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
  # VMs — three members. The weft-ha-postgresql agent elects the leader
  # at boot via etcd ; the framework does not pin a primary index. Caddy
  # in weft-agent routes 5432 to whichever member's :8008/primary
  # returns 200.
  # -----------------------------------------------------------------

  vm "postgres" {
    image    = "ghcr.io/openweft/postgres-ha:v0.2.0"
    replicas = 3
    cpu      = 4
    mem_mb   = 8192
    disk_gb  = 20
    network  = "postgres"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "superuser_password"   { env_name = "POSTGRES_SUPERUSER_PASSWORD" }
    env_from "replication_password" { env_name = "POSTGRES_REPLICATION_PASSWORD" }
    env_from "database_name"        { env_name = "POSTGRES_INITIAL_DATABASE" }
    env_from "synchronous_commit"   { env_name = "POSTGRES_SYNCHRONOUS_COMMIT" }
    env_from "fence_timeout_sec"    { env_name = "WEFT_HA_PG_FENCE_TIMEOUT_SEC" }
    env_from "etcd_session_ttl_sec" { env_name = "WEFT_HA_PG_ETCD_TTL_SEC" }

    volume "data" {
      size_gib = 50
      format   = "raw"
      mount    = "/var/lib/postgresql"
    }
  }
}
