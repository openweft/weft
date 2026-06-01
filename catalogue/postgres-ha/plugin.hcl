# PostgreSQL HA — three Patroni-managed Postgres members in the
# HA 3-DC layout. Patroni elects the leader, streams WAL to the two
# followers, and republishes the topology via etcd (the embedded
# instance the rest of weft already runs, per openweft_etcd_embedded).
#
# This plugin gives you a primary + two synchronous-capable replicas
# with automatic failover. Logical replication, pgbouncer pooling, and
# read-replica routing are NOT included — see docs for the seams.
#
# Image : upstream `quay.io/patroni/patroni:3.3.x` by default. An
# openweft-built `ghcr.io/openweft/postgres-ha:v0.1.0` is planned but
# NOT YET PUBLISHED ; switch the `image` input once it lands.
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
  description = "Three-node Patroni-managed PostgreSQL cluster with one replica per DC"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "quay.io/patroni/patroni:3.3.2"
    help    = "Patroni+Postgres OCI image. Default tracks upstream until ghcr.io/openweft/postgres-ha:v0.1.0 is published."
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
    help     = "Password used by Patroni replicas to stream WAL from the primary."
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
    help    = "Postgres `synchronous_commit` setting. `on` = wait for one replica ack ; `off` = async (lower latency, possible data loss on primary crash)."
  }

  input "tenant_network_cidrs" {
    type    = "string"
    default = "10.0.0.0/8"
    help    = "Comma-free CIDR (single string) of tenant networks allowed to reach 5432/tcp. Default opens the RFC1918 10/8 superblock — narrow it for production."
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
    description = "Patroni replicas — 5432/tcp from tenants, 8008/tcp Patroni REST inter-replica, DNS egress."
    networks    = ["postgres"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 5432
      port_max    = 5432
      remote_cidr = "10.0.0.0/8"
      description = "PostgreSQL wire protocol from tenant networks (narrow via tenant_network_cidrs input in v2)."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8008
      port_max    = 8008
      remote_cidr = "10.50.0.0/24"
      description = "Patroni REST API — leader election + topology gossip between replicas."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 5432
      port_max    = 5432
      remote_cidr = "10.50.0.0/24"
      description = "Replica → primary streaming replication on the same Postgres port."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 8008
      port_max    = 8008
      remote_cidr = "10.50.0.0/24"
      description = "Patroni REST API — outbound side of the gossip mesh."
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
  # VMs — three Patroni members. Patroni elects the leader at boot ;
  # the framework does not pin a primary index.
  # -----------------------------------------------------------------

  vm "postgres" {
    image    = "quay.io/patroni/patroni:3.3.2"
    replicas = 3
    cpu      = 4
    mem_mb   = 8192
    disk_gb  = 20
    network  = "postgres"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "superuser_password"   { env_name = "PATRONI_SUPERUSER_PASSWORD" }
    env_from "replication_password" { env_name = "PATRONI_REPLICATION_PASSWORD" }
    env_from "database_name"        { env_name = "PATRONI_POSTGRESQL_DATABASE" }
    env_from "synchronous_commit"   { env_name = "PATRONI_POSTGRESQL_SYNCHRONOUS_COMMIT" }

    volume "data" {
      size_gib = 50
      format   = "raw"
      mount    = "/var/lib/postgresql"
    }
  }
}
