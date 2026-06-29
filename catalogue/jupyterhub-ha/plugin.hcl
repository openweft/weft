# JupyterHub HA — per-user microVM Jupyter notebooks with a
# 3-DC controller plane.
#
# The Hub itself is stateless ; state lives in CockroachDB
# (default) or SQLite-on-NFS (small deploys). Per-user notebooks
# are spawned by a custom Spawner (spawner/weft_spawner.py) that
# shells `weft instance ...` against the host agent's socket.
#
# Operator pre-flight (see docs/catalogue/jupyterhub-ha.md):
#   1. Create a weft project for user VMs and set quotas sized
#      for the user count — per-project hard caps landed in
#      88cece7c6, an unsized project means user spawn fails with
#      codes.ResourceExhausted on the first quota-breaching login.
#   2. Register an OIDC client at the same issuer weft trusts
#      (Dex / Keycloak / Okta). Redirect URI :
#      https://<domain>/hub/oauth_callback
#   3. weft plugin install jupyterhub-ha \
#        --project infra \
#        --input oidc_issuer=https://dex.example.com \
#        --input oidc_client_id=jupyterhub \
#        --input oidc_client_secret=$JH_CLIENT_SECRET \
#        --input domain=hub.example.com \
#        --input project_uuid=<user-vm-project-uuid>

plugin "jupyterhub-ha" {
  version     = "v1"
  kind        = "portal"
  description = "JupyterHub HA portal — per-user microVM notebooks, 3-DC controllers, OIDC."
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "oidc_issuer" {
    type     = "string"
    required = true
    help     = "OIDC issuer URL (Dex/Keycloak/Okta). Same one weft itself trusts."
  }

  input "oidc_client_id" {
    type     = "string"
    required = true
    help     = "OAuth2 client ID registered with the issuer for the Hub."
  }

  input "oidc_client_secret" {
    type     = "string"
    required = true
    secret   = true
    help     = "OAuth2 client secret. Cluster secret store, never plain HCL."
  }

  input "domain" {
    type     = "string"
    required = true
    help     = "FQDN the Hub answers on (e.g. hub.example.com). Caddy routes this to the controllers."
  }

  input "project_uuid" {
    type     = "string"
    required = true
    help     = "weft project UUID that owns user notebook VMs. MUST have tenant quotas set."
  }

  input "image" {
    type    = "string"
    default = "quay.io/jupyter/minimal-notebook:python-3.12"
    help    = "OCI image for user notebook microVMs. Stopgap until ghcr.io/openweft/jupyter-user:v0.1.0 is published."
  }

  input "cpu_per_user" {
    type    = "int"
    default = "2"
    help    = "vCPU count per user notebook microVM."
  }

  input "memory_gib_per_user" {
    type    = "int"
    default = "4"
    help    = "Memory (GiB) per user notebook microVM."
  }

  input "home_volume_gib" {
    type    = "int"
    default = "50"
    help    = "Persistent /home/jovyan volume size per user (reflink-snapshottable)."
  }

  input "idle_minutes" {
    type    = "int"
    default = "60"
    help    = "Cull idle notebooks after this many minutes. VM is stopped, not destroyed."
  }

  input "db_backend" {
    type    = "string"
    default = "cockroach"
    help    = "Hub state backend: 'cockroach' (HA, 3-DC) or 'sqlite-nfs' (small deploys)."
  }

  input "admin_group" {
    type    = "string"
    default = "weft:admin"
    help    = "OIDC group that grants Hub admin privileges."
  }

  input "user_group" {
    type    = "string"
    default = ""
    help    = "OIDC group permitted to log in. Convention: weft:project:<project_uuid>. Empty = any authenticated user."
  }

  # -----------------------------------------------------------------
  # Network — controllers + user VMs share one /24 inside this network.
  # -----------------------------------------------------------------

  network "jupyterhub-control" {
    cidr = "10.255.42.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "hub-controller" {
    description = "JupyterHub controllers — 8000/tcp from proxy, 8888 out to user VMs."
    networks    = ["jupyterhub-control"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8000
      port_max    = 8000
      remote_cidr = "0.0.0.0/0"
      description = "Caddy proxy → Hub HTTP."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8081
      port_max    = 8081
      remote_cidr = "10.255.42.0/24"
      description = "Inter-controller hub API."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 8888
      port_max    = 8888
      remote_cidr = "10.255.42.0/24"
      description = "Hub → user notebook server."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "OIDC discovery + token exchange."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 26257
      port_max    = 26257
      remote_cidr = "10.255.42.0/24"
      description = "CockroachDB (skip when db_backend=sqlite-nfs)."
    }
  }

  security_group "user-notebook" {
    description = "Per-user notebook VMs — only the hub controllers reach 8888/tcp."
    networks    = ["jupyterhub-control"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8888
      port_max    = 8888
      remote_cidr = "10.255.42.0/24"
      description = "Hub → notebook server."
    }
    # No egress rules by default — user notebooks are network-
    # isolated. Operators add egress here if their workflow needs
    # PyPI / conda mirrors etc.
  }

  security_group "jupyterhub-db" {
    description = "CockroachDB replicas — 26257 from controllers, 8080 admin from controllers, gossip between replicas."
    networks    = ["jupyterhub-control"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 26257
      port_max    = 26257
      remote_cidr = "10.255.42.0/24"
      description = "SQL + gossip."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8080
      port_max    = 8080
      remote_cidr = "10.255.42.0/24"
      description = "Cockroach admin UI / health."
    }
  }

  # -----------------------------------------------------------------
  # VMs — controllers (always) + db replicas (when db_backend=cockroach).
  # User notebook VMs are spawned lazily by spawner/weft_spawner.py
  # and are NOT declared here.
  # -----------------------------------------------------------------

  vm "controller" {
    # Bespoke image with our spawner installed ; built from
    # catalogue/jupyterhub-ha/Dockerfile, published by the
    # openweft/jupyterhub-ha repo's CI on tag.
    image    = "ghcr.io/openweft/jupyterhub-ha:v0.1.0"
    runtime  = "microvm"
    replicas = 3
    cpu      = 2
    mem_mb   = 2048
    disk_gb  = 10
    network  = "jupyterhub-control"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "oidc_issuer"        { env_name = "OIDC_ISSUER" }
    env_from "oidc_client_id"     { env_name = "OIDC_CLIENT_ID" }
    env_from "oidc_client_secret" { env_name = "OIDC_CLIENT_SECRET" }
    env_from "domain"             { env_name = "DOMAIN" }
    env_from "project_uuid"       { env_name = "WEFT_PROJECT_UUID" }
    env_from "image"              { env_name = "JUPYTER_USER_IMAGE" }
    env_from "cpu_per_user"       { env_name = "CPU_PER_USER" }
    env_from "memory_gib_per_user" { env_name = "MEMORY_GIB_PER_USER" }
    env_from "home_volume_gib"    { env_name = "HOME_VOLUME_GIB" }
    env_from "idle_minutes"       { env_name = "IDLE_MINUTES" }
    env_from "db_backend"         { env_name = "DB_BACKEND" }
    env_from "admin_group"        { env_name = "ADMIN_GROUP" }
    env_from "user_group"         { env_name = "USER_GROUP" }

    # Mount the host's weft agent socket into the controller so
    # the spawner can shell `weft instance ...`. Same-host model
    # only — on macOS this is virtio-9p (per qemu_microvm_9p), on
    # Linux it's virtio-fs.
    share "weft-sock" {
      host_path  = "/var/run/weft"
      guest_path = "/run/weft"
      mode       = "ro"
    }
  }

  vm "db" {
    # Skipped when db_backend != "cockroach" ; the deployer reads
    # this `enabled_if` expression against the resolved `input`
    # namespace before materialising the block.
    enabled_if = input.db_backend == "cockroach"
    image      = "cockroachdb/cockroach:v24.2.0"
    runtime  = "microvm"
    replicas   = 3
    cpu        = 2
    mem_mb     = 4096
    disk_gb    = 20
    network    = "jupyterhub-control"

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
  # one controller (JupyterHub WebSocket upgrades don't survive
  # cross-controller hops without that).
  # -----------------------------------------------------------------

  proxy_route "hub" {
    host        = input.domain
    upstreams   = ["controller-1:8000", "controller-2:8000", "controller-3:8000"]
    sticky      = "cookie:jupyterhub-session-id"
    health_path = "/hub/health"
    websocket   = true
  }
}
