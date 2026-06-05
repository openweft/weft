# iRODS HA — three iRODS catalog service provider replicas (iCAT-enabled
# servers) sharing a single Postgres-backed catalog, one provider per DC.
# Each provider runs the upstream iRODS server (BSD-3-Clause) plus the
# openweft-built `weft-ha-irods` Go agent. The agent :
#
#   - bootstraps the iRODS zone the first time a provider comes up
#     (creates the iCAT schema in the shared Postgres, mints
#     negotiation_key / control_plane_key / zone_key, seeds them in
#     etcd so the other two providers pick them up instead of
#     re-minting and ending up with a split-zone),
#   - reconciles /etc/irods/server_config.json + core.re tweaks the
#     operator drives via plugin inputs (no SSH'ing into the guest),
#     and runs `irods-grid` health checks against the local server,
#   - exposes a role API at :8009 (`/ready`, `/zone`) the L4 Caddy in
#     weft-agent active-probes ; iRODS clients connect on 1247/tcp and
#     Caddy routes to whichever provider is currently healthy.
#
# iRODS itself is stateless once the iCAT is in place — failover is a
# load-balancer drain, NOT a leader election. The data plane (storage
# resources) is also stateless from the provider's POV : per-DC resource
# servers (consumers) hold the bytes, replication strategy is configured
# inside iRODS via the `irods-msi` rule engine.
#
# The shared catalog Postgres is NOT in this plugin's scope — install
# `postgres-ha` first and point this plugin at its zone-internal address.
# Coupling the two would force a hard ordering on the operator and lock
# them out of running iRODS against an existing Postgres they already
# manage.
#
# Image : `ghcr.io/openweft/irods-ha:v0.1.0` — upstream iRODS server
# 5.x + the openweft Go agent in one rootfs. License : BSD-3-Clause
# (iRODS) + BSD-3-Clause (openweft) — fully permissive.
#
# Operator pre-flight (see docs/catalogue/irods-ha.md) :
#   1. Install postgres-ha into the same project — note the cluster DNS
#      name (`postgres-ha-<short>.weft`).
#   2. Pick a zone name (the iRODS namespace ; "weftZone" by default),
#      an rodsadmin password, and the catalog DB password.
#   3. weft plugin install irods-ha \
#        --project data \
#        --input zone_name=weftZone \
#        --input admin_password=$IRODS_ADMIN \
#        --input icat_db_host=postgres-ha-abc.weft \
#        --input icat_db_password=$ICAT_PWD

plugin "irods-ha" {
  version     = "v1"
  kind        = "data-management"
  description = "Three iRODS catalog providers on a shared Postgres catalog, one per DC. Managed by the weft-ha-irods Go agent (zone bootstrap, key sync, health probe)."
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "ghcr.io/openweft/irods-ha:v0.1.0"
    help    = "iRODS server + weft-ha-irods agent OCI image. The Go agent + upstream irods-server 5.x in one rootfs."
  }

  input "zone_name" {
    type    = "string"
    default = "weftZone"
    help    = "iRODS zone name — the namespace clients address (irods://<user>@<zone>:1247/<path>). Immutable once the iCAT is bootstrapped."
  }

  input "admin_password" {
    type     = "string"
    required = true
    secret   = true
    help     = "Password for the bootstrap `rodsadmin` user. Cluster secret store, never plain HCL. Rotate via `iadmin moduser rodsadmin password ...` once the zone is live."
  }

  input "icat_db_host" {
    type     = "string"
    required = true
    help     = "Catalog Postgres host the providers connect to. Typically the in-zone DNS of a postgres-ha install (e.g. `postgres-ha-<short>.weft`). Must already exist — this plugin does NOT install Postgres."
  }

  input "icat_db_port" {
    type    = "int"
    default = "5432"
    help    = "Catalog Postgres port."
  }

  input "icat_db_name" {
    type    = "string"
    default = "ICAT"
    help    = "Catalog database name. The agent creates it on first bootstrap if missing (requires icat_db_user to have CREATEDB)."
  }

  input "icat_db_user" {
    type    = "string"
    default = "irods"
    help    = "Catalog Postgres user. The agent expects this role to already exist with login + CREATEDB rights on the target Postgres."
  }

  input "icat_db_password" {
    type     = "string"
    required = true
    secret   = true
    help     = "Catalog Postgres password for icat_db_user. Cluster secret store."
  }

  input "negotiation_key" {
    type    = "string"
    default = ""
    secret  = true
    help    = "32-character pre-shared key for the iRODS native auth client/server SSL negotiation. Empty → the agent mints one during bootstrap and seeds it via etcd so all three providers agree."
  }

  input "control_plane_key" {
    type    = "string"
    default = ""
    secret  = true
    help    = "32-character pre-shared key for the iRODS control-plane (grid admin) protocol. Empty → minted by the agent (same flow as negotiation_key)."
  }

  input "zone_key" {
    type    = "string"
    default = ""
    secret  = true
    help    = "Inter-zone trust key. Empty → minted on bootstrap. Required if you plan to federate this zone with another iRODS zone."
  }

  input "resource_volume_gib" {
    type    = "int"
    default = "100"
    help    = "Per-provider local resource volume (vault for the per-DC default storage resource). Object content lives here ; the catalog only stores metadata + pointers."
  }

  input "tenant_network_cidrs" {
    type    = "string"
    default = "10.0.0.0/8"
    help    = "Comma-free CIDR (single string) of tenant networks allowed to reach 1247/tcp. Default opens RFC1918 10/8 — narrow for production."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "irods" {
    cidr = "10.54.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "irods-zone" {
    description = "iRODS providers — 1247/tcp (XMSG main port) from tenants, 1248/tcp control-plane between providers, 20000-20199/tcp parallel transfer port range, 8009/tcp role API for Caddy probes."
    networks    = ["irods"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 1247
      port_max    = 1247
      remote_cidr = "10.0.0.0/8"
      description = "iRODS main protocol (XMSG) from tenant networks. Narrow via tenant_network_cidrs."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 1248
      port_max    = 1248
      remote_cidr = "10.54.0.0/24"
      description = "iRODS control plane (irods-grid admin protocol) between providers."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 20000
      port_max    = 20199
      remote_cidr = "10.0.0.0/8"
      description = "Parallel-transfer dynamic port range — clients open a second connection per stream for high-throughput PUT/GET."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8009
      port_max    = 8009
      remote_cidr = "10.0.0.0/8"
      description = "Role API (/ready, /zone) for the L4 Caddy in weft-agent's active health check."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 5432
      port_max    = 5432
      remote_cidr = "10.50.0.0/24"
      description = "Outbound to the catalog Postgres (postgres-ha network)."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 1248
      port_max    = 1248
      remote_cidr = "10.54.0.0/24"
      description = "Outbound control plane to peer providers."
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
  # VMs — three providers. The agent picks bootstrap leader via etcd
  # advisory lock ; once the iCAT schema is in place, every provider
  # is read-write equivalent and the L4 Caddy routes 1247 to any of
  # them that pass /ready.
  # -----------------------------------------------------------------

  vm "irods" {
    image    = "ghcr.io/openweft/irods-ha:v0.1.0"
    replicas = 3
    cpu      = 4
    mem_mb   = 8192
    disk_gb  = 20
    network  = "irods"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "zone_name"         { env_name = "IRODS_ZONE_NAME" }
    env_from "admin_password"    { env_name = "IRODS_ADMIN_PASSWORD" }
    env_from "icat_db_host"      { env_name = "IRODS_ICAT_DB_HOST" }
    env_from "icat_db_port"      { env_name = "IRODS_ICAT_DB_PORT" }
    env_from "icat_db_name"      { env_name = "IRODS_ICAT_DB_NAME" }
    env_from "icat_db_user"      { env_name = "IRODS_ICAT_DB_USER" }
    env_from "icat_db_password"  { env_name = "IRODS_ICAT_DB_PASSWORD" }
    env_from "negotiation_key"   { env_name = "IRODS_NEGOTIATION_KEY" }
    env_from "control_plane_key" { env_name = "IRODS_CONTROL_PLANE_KEY" }
    env_from "zone_key"          { env_name = "IRODS_ZONE_KEY" }

    volume "vault" {
      size_gib = 100
      format   = "raw"
      mount    = "/var/lib/irods/Vault"
    }
  }
}
