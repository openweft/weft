# Forgejo HA — three Forgejo (Git forge ; AGPLv3+, soft-fork of Gitea)
# replicas on a shared Postgres catalog + shared object storage for
# attachments + LFS, one replica per DC. The openweft-built
# `weft-ha-forgejo` Go agent runs alongside each Forgejo instance to :
#
#   - bootstrap the install (first replica creates schema + admin user
#     + minted secrets ; the other two join an already-initialised
#     instance instead of racing to install),
#   - seed the shared secrets (SECRET_KEY, INTERNAL_TOKEN, OAUTH2_JWT_SECRET,
#     LFS_JWT_SECRET) via etcd so all three replicas agree — a
#     mismatched SECRET_KEY corrupts session cookies + 2FA storage
#     silently,
#   - expose a role API at :3001 (`/ready`, `/info`) the L7 Caddy in
#     weft-agent active-probes ; client traffic on :3000 (HTTP) and
#     :2222 (SSH for git push/pull) is load-balanced across whichever
#     replicas pass /ready.
#
# Distinct from `forgejo-runners-ha`. That plugin runs CI workers
# (act_runner) ; THIS plugin runs the Git forge itself. Operators
# typically install both — runners point at the Forgejo URL this
# plugin exposes.
#
# **License caveat** : Forgejo is AGPLv3+. Hosting Forgejo for users
# accessible over a network triggers the "source disclosure" clause of
# AGPL §13. openweft itself is BSD-3-Clause ; we are merely packaging
# Forgejo's upstream binary unmodified. If you ship a fork with
# modifications, your fork inherits the AGPL obligation.
#
# Backends required before install :
#   - `postgres-ha` for the Forgejo schema (recommended) — Forgejo also
#     supports SQLite + MySQL but the HA story only makes sense with
#     a clustered Postgres.
#   - object storage for attachments + LFS — `versitygw-ha` works
#     (Apache-2.0, S3 API) ; the operator points Forgejo at it via the
#     `storage_*` inputs.
#
# Image : `codeberg.org/forgejo/forgejo:10` upstream + the openweft
# Go agent dropped in via the OCI image's `/usr/local/bin`. We do NOT
# fork Forgejo.
#
# Operator pre-flight (see docs/catalogue/forgejo-ha.md):
#   1. Install postgres-ha (or point at an existing Postgres-HA).
#   2. Install versitygw-ha (or point at an external S3 service).
#   3. Pick an admin username + admin password ; pick a domain.
#   4. weft plugin install forgejo-ha \
#        --project devtools \
#        --input domain=git.example.com \
#        --input admin_username=root \
#        --input admin_password=$FORGEJO_ADMIN \
#        --input admin_email=root@example.com \
#        --input db_host=postgres-ha-abc.weft \
#        --input db_password=$DB_PWD \
#        --input s3_endpoint=https://versitygw-ha-xyz.weft:7070 \
#        --input s3_access_key=$S3_AK \
#        --input s3_secret_key=$S3_SK

plugin "forgejo-ha" {
  version     = "v1"
  kind        = "git-forge"
  description = "Three Forgejo Git-forge replicas behind Caddy with shared Postgres + S3 storage, one per DC. Managed by the weft-ha-forgejo Go agent (install bootstrap, secret sync, health probe)."
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "codeberg.org/forgejo/forgejo:10"
    help    = "Forgejo OCI image. AGPLv3+ upstream — we ship the agent next to it, not on top of it."
  }

  input "domain" {
    type     = "string"
    required = true
    help     = "Public hostname Forgejo serves (sets ROOT_URL + SSH host key). Must resolve to the Caddy edge listener."
  }

  input "admin_username" {
    type     = "string"
    required = true
    help     = "Bootstrap admin username. The agent creates it on first run if missing."
  }

  input "admin_password" {
    type     = "string"
    required = true
    secret   = true
    help     = "Bootstrap admin password. Rotate via Forgejo's web UI once the install is live."
  }

  input "admin_email" {
    type     = "string"
    required = true
    help     = "Bootstrap admin email. Used as the From address for password-reset mail until SMTP is reconfigured."
  }

  input "db_host" {
    type     = "string"
    required = true
    help     = "Catalog Postgres host (typically `postgres-ha-<short>.weft`). Postgres must already be installed."
  }

  input "db_port" {
    type    = "int"
    default = "5432"
    help    = "Catalog Postgres port."
  }

  input "db_name" {
    type    = "string"
    default = "forgejo"
    help    = "Catalog Postgres database name. The agent creates it on first bootstrap if missing."
  }

  input "db_user" {
    type    = "string"
    default = "forgejo"
    help    = "Catalog Postgres user. Must already exist with login + CREATEDB on the target Postgres."
  }

  input "db_password" {
    type     = "string"
    required = true
    secret   = true
    help     = "Catalog Postgres password."
  }

  input "s3_endpoint" {
    type    = "string"
    default = ""
    help    = "S3 endpoint URL for attachments + LFS (e.g. https://versitygw-ha-<short>.weft:7070). Empty → local disk on each replica (NOT HA — only for single-host dev installs)."
  }

  input "s3_access_key" {
    type    = "string"
    default = ""
    help    = "S3 access-key-id. Required when s3_endpoint is set."
  }

  input "s3_secret_key" {
    type    = "string"
    default = ""
    secret  = true
    help    = "S3 secret-access-key. Required when s3_endpoint is set."
  }

  input "s3_bucket" {
    type    = "string"
    default = "forgejo"
    help    = "S3 bucket for attachments + LFS. The agent creates it on first bootstrap."
  }

  input "secret_key" {
    type    = "string"
    default = ""
    secret  = true
    help    = "Master encryption key for the Forgejo install (`SECRET_KEY` ini option). Empty → minted by the bootstrap leader and seeded via etcd. ALL replicas must agree — a mismatch silently corrupts session cookies."
  }

  input "internal_token" {
    type    = "string"
    default = ""
    secret  = true
    help    = "Internal API token (`INTERNAL_TOKEN`). Empty → minted + seeded. Required identical across replicas."
  }

  input "lfs_jwt_secret" {
    type    = "string"
    default = ""
    secret  = true
    help    = "LFS JWT signing secret. Empty → minted + seeded. Required identical across replicas."
  }

  input "smtp_url" {
    type    = "string"
    default = ""
    help    = "Outbound SMTP URL (smtp+starttls://user:pass@host:port). Empty leaves mail disabled — password reset will not work."
  }

  input "data_volume_gib" {
    type    = "int"
    default = "20"
    help    = "Per-replica persistent volume for /var/lib/forgejo (repo cache, SSH host keys, custom templates). Repository content + LFS / attachments live in S3, NOT here."
  }

  input "tenant_network_cidrs" {
    type    = "string"
    default = "10.0.0.0/8"
    help    = "Comma-free CIDR allowed to reach 3000/tcp + 2222/tcp. Narrow for production."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "forgejo" {
    cidr = "10.55.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "forgejo-forge" {
    description = "Forgejo — 3000/tcp HTTP from tenants (Caddy edge fronts this), 2222/tcp git SSH, 3001/tcp role API for active probes, outbound to Postgres + S3 + SMTP."
    networks    = ["forgejo"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 3000
      port_max    = 3000
      remote_cidr = "10.0.0.0/8"
      description = "Forgejo HTTP from the Caddy edge listener."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 2222
      port_max    = 2222
      remote_cidr = "10.0.0.0/8"
      description = "Git SSH (custom port 2222 to leave the host's 22 alone). The Caddy L4 plane routes :22 → :2222 ; users see `git@git.example.com`."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 3001
      port_max    = 3001
      remote_cidr = "10.0.0.0/8"
      description = "Role API for the L7 Caddy active probe."
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
      port_min    = 7070
      port_max    = 7070
      remote_cidr = "10.51.0.0/24"
      description = "Outbound to versitygw-ha (S3) for attachments + LFS."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 587
      port_max    = 587
      remote_cidr = "0.0.0.0/0"
      description = "Outbound SMTP submission (when smtp_url is set)."
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
  # VMs — three replicas behind Caddy. No leader election ; replicas
  # are stateless from Forgejo's POV once SECRET_KEY/INTERNAL_TOKEN/
  # LFS_JWT_SECRET match across the install.
  # -----------------------------------------------------------------

  vm "forgejo" {
    image    = "codeberg.org/forgejo/forgejo:10"
    replicas = 3
    cpu      = 2
    mem_mb   = 4096
    disk_gb  = 10
    network  = "forgejo"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "domain"          { env_name = "FORGEJO__server__DOMAIN" }
    env_from "admin_username"  { env_name = "FORGEJO_ADMIN_USERNAME" }
    env_from "admin_password"  { env_name = "FORGEJO_ADMIN_PASSWORD" }
    env_from "admin_email"     { env_name = "FORGEJO_ADMIN_EMAIL" }
    env_from "db_host"         { env_name = "FORGEJO__database__HOST" }
    env_from "db_port"         { env_name = "FORGEJO__database__PORT" }
    env_from "db_name"         { env_name = "FORGEJO__database__NAME" }
    env_from "db_user"         { env_name = "FORGEJO__database__USER" }
    env_from "db_password"     { env_name = "FORGEJO__database__PASSWD" }
    env_from "s3_endpoint"     { env_name = "FORGEJO__storage__MINIO_ENDPOINT" }
    env_from "s3_access_key"   { env_name = "FORGEJO__storage__MINIO_ACCESS_KEY_ID" }
    env_from "s3_secret_key"   { env_name = "FORGEJO__storage__MINIO_SECRET_ACCESS_KEY" }
    env_from "s3_bucket"       { env_name = "FORGEJO__storage__MINIO_BUCKET" }
    env_from "secret_key"      { env_name = "FORGEJO__security__SECRET_KEY" }
    env_from "internal_token"  { env_name = "FORGEJO__security__INTERNAL_TOKEN" }
    env_from "lfs_jwt_secret"  { env_name = "FORGEJO__server__LFS_JWT_SECRET" }
    env_from "smtp_url"        { env_name = "FORGEJO_SMTP_URL" }

    volume "data" {
      size_gib = 20
      format   = "raw"
      mount    = "/var/lib/forgejo"
    }
  }
}
