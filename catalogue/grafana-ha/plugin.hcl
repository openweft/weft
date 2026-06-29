# Grafana HA — three Grafana replicas behind the existing Caddy proxy
# plane, OIDC auth, shared session DB.
#
# Grafana ships HA out of the box ; the only requirement is a shared
# database for sessions, dashboards, datasources, and alert state.
# This plugin defaults to **CockroachDB** (3 replicas spread across
# the same 3 DCs, the same pattern catalogue/jupyterhub-ha uses) and
# leaves an escape hatch for SQLite-on-NFS for sub-50-user deploys.
#
# Sticky sessions on the Caddy proxy keep a logged-in user pinned to
# one Grafana replica — alerting and live tail rely on in-memory state
# that doesn't survive cross-replica hops without it. The Hub-style
# proxy_route block emits `sticky = "cookie:grafana_session"`.
#
# Image : upstream `grafana/grafana-oss:11.6` by default. No openweft
# fork.
#
# Operator pre-flight (see docs/catalogue/grafana-ha.md):
#   1. Pick the admin password (used only for the local-admin escape
#      hatch — OIDC handles day-to-day login).
#   2. Register an OIDC client at the same issuer weft trusts.
#      Redirect URI : https://<domain>/login/generic_oauth
#   3. Decide on db_backend : 'cockroach' (default, HA) or
#      'sqlite-nfs' (small deploys).
#   4. weft plugin install grafana-ha \
#        --project observability \
#        --input admin_password=$GF_ADMIN \
#        --input oidc_client_id=grafana \
#        --input oidc_client_secret=$GF_OIDC \
#        --input domain=grafana.example.com

plugin "grafana-ha" {
  version     = "v1"
  kind        = "dashboards"
  description = "Three Grafana replicas behind Caddy with sticky sessions, OIDC auth, CockroachDB-backed state"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "grafana/grafana-oss:11.6"
    help    = "Grafana OSS OCI image. No openweft fork — tracks upstream grafana/grafana-oss."
  }

  input "admin_password" {
    type     = "string"
    required = true
    secret   = true
    help     = "Local 'admin' password (GF_SECURITY_ADMIN_PASSWORD). Used only when OIDC is unavailable ; rotate after first login."
  }

  input "oidc_issuer" {
    type    = "string"
    default = ""
    help    = "OIDC issuer URL (Dex/Keycloak/Okta). Empty = same as weft's own issuer ; the in-guest agent reads weft cluster config."
  }

  input "oidc_client_id" {
    type     = "string"
    required = true
    help     = "OAuth2 client ID registered with the issuer for Grafana."
  }

  input "oidc_client_secret" {
    type     = "string"
    required = true
    secret   = true
    help     = "OAuth2 client secret. Cluster secret store, never plain HCL."
  }

  input "db_backend" {
    type    = "string"
    default = "cockroach"
    help    = "State backend: 'cockroach' (HA, 3-DC) or 'sqlite-nfs' (small deploys ; same escape hatch as jupyterhub-ha)."
  }

  input "domain" {
    type     = "string"
    required = true
    help     = "FQDN Grafana answers on (e.g. grafana.example.com). Caddy routes this to the replicas with cookie-stickiness."
  }

  input "admin_group" {
    type    = "string"
    default = "weft:admin"
    help    = "OIDC group that maps to Grafana 'Admin' role on first login."
  }

  input "viewer_group" {
    type    = "string"
    default = ""
    help    = "OIDC group permitted to log in as Viewer. Empty = any authenticated user."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "dashboards" {
    cidr = "10.62.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "grafana-dashboards" {
    description = "Grafana replicas — 3000/tcp from the Caddy proxy plane, 26257 egress to Cockroach (when db_backend=cockroach), 443 egress for OIDC + datasources."
    networks    = ["dashboards"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 3000
      port_max    = 3000
      remote_cidr = "10.54.0.0/24"
      description = "Caddy edge → Grafana. The Caddy plugin lives on 10.54.0.0/24 ; widen to 0.0.0.0/0 only if you front Grafana directly."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 9094
      port_max    = 9094
      remote_cidr = "10.62.0.0/24"
      description = "Grafana unified alerting HA gossip (memberlist) between replicas."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 9094
      port_max    = 9094
      remote_cidr = "10.62.0.0/24"
      description = "Outbound side of the unified-alerting gossip mesh."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 26257
      port_max    = 26257
      remote_cidr = "10.62.0.0/24"
      description = "CockroachDB SQL (skip / no-op when db_backend=sqlite-nfs)."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "HTTPS egress for OIDC discovery + external datasources (CloudWatch, Grafana Cloud, …)."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 9090
      port_max    = 9090
      remote_cidr = "10.60.0.0/24"
      description = "Prometheus datasource — on-cluster prometheus-ha."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 3100
      port_max    = 3100
      remote_cidr = "10.61.0.0/24"
      description = "Loki datasource — on-cluster loki-ha."
    }

    rule "egress" {
      protocol    = "udp"
      port_min    = 53
      port_max    = 53
      remote_cidr = "0.0.0.0/0"
      description = "DNS."
    }
  }

  security_group "grafana-db" {
    description = "CockroachDB replicas for Grafana state — 26257 SQL + gossip, 8080 admin UI."
    networks    = ["dashboards"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 26257
      port_max    = 26257
      remote_cidr = "10.62.0.0/24"
      description = "SQL + inter-replica gossip."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8080
      port_max    = 8080
      remote_cidr = "10.62.0.0/24"
      description = "Cockroach admin UI / health."
    }
  }

  # -----------------------------------------------------------------
  # VMs — Grafana replicas + (optional) CockroachDB state tier.
  # The user-supplied `db_backend` selects between cockroach and
  # sqlite-on-NFS at install time ; the SQLite path skips the `db`
  # block via the same `enabled_if` shape catalogue/jupyterhub-ha
  # uses (see openweft/jupyterhub-ha for the SQLite-NFS recipe).
  # -----------------------------------------------------------------

  vm "grafana" {
    image    = "grafana/grafana-oss:11.6"
    runtime  = "microvm"
    replicas = 3
    cpu      = 2
    mem_mb   = 4096
    disk_gb  = 20
    network  = "dashboards"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "admin_password"     { env_name = "GF_SECURITY_ADMIN_PASSWORD" }
    env_from "oidc_issuer"        { env_name = "GF_AUTH_GENERIC_OAUTH_ISSUER" }
    env_from "oidc_client_id"     { env_name = "GF_AUTH_GENERIC_OAUTH_CLIENT_ID" }
    env_from "oidc_client_secret" { env_name = "GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET" }
    env_from "db_backend"         { env_name = "GF_DATABASE_BACKEND" }
    env_from "domain"             { env_name = "GF_SERVER_DOMAIN" }
    env_from "admin_group"        { env_name = "GF_OIDC_ADMIN_GROUP" }
    env_from "viewer_group"       { env_name = "GF_OIDC_VIEWER_GROUP" }
  }

  vm "db" {
    # Skipped when db_backend != "cockroach" (sqlite-nfs path) ;
    # the installer reads this expression against the resolved input
    # namespace before materialising the block. Same shape as
    # catalogue/jupyterhub-ha.
    enabled_if = input.db_backend == "cockroach"
    image      = "cockroachdb/cockroach:v24.2.0"
    runtime  = "microvm"
    replicas   = 3
    cpu        = 2
    mem_mb     = 4096
    disk_gb    = 20
    network    = "dashboards"

    placement {
      az   = "different"
      host = "different"
    }

    volume "data" {
      size_gib = 100
      format   = "raw"
      mount    = "/cockroach/cockroach-data"
    }
  }

  # -----------------------------------------------------------------
  # Caddy route — sticky-by-cookie keeps a given browser pinned to
  # one Grafana replica. Live tail, unified-alerting UI, and Explore
  # all rely on in-memory state that doesn't survive replica hops.
  # -----------------------------------------------------------------

  proxy_route "grafana" {
    host        = input.domain
    upstreams   = ["grafana-0:3000", "grafana-1:3000", "grafana-2:3000"]
    sticky      = "cookie:grafana_session"
    health_path = "/api/health"
    websocket   = true
  }
}
