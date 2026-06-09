# Loom HA — three weft-loom-server replicas (collaborative editor + sandbox
# compile orchestrator, openweft-native, Overleaf-generic) deployed one per
# DC. Caddy L7 fronts the trio with active-probes against /api/healthz ;
# operators reach loom via the same SSO (dex OIDC) the rest of the platform
# uses.
#
# Loom V0.2 is in-memory (per-room Yjs CRDT broadcast) + local file store
# under /var/lib/weft-loom. The HA layer here gives :
#   - host fan-out for users (3 endpoints behind Caddy LB),
#   - one DC dying doesn't take the editor offline (the other two serve
#     existing rooms + accept new ones),
#   - room state is best-effort recoverable from the per-replica file
#     store once the original DC comes back ; cross-DC persistence lands
#     in V0.3 (weft-block volume + S3-backed project store).
#
# **What this plugin does NOT yet provide** (V0.3 follow-ups documented in
# weft-loom-server's STATUS) :
#   - cross-replica project store sync (today each replica only sees its
#     own /var/lib/weft-loom),
#   - durable PDF preview cache,
#   - dex OIDC verification (the server's auth abstraction shipped in
#     V0.2 as StaticVerifier ; the dex wiring lands in V0.3).
#
# **Today's recommended use** : single-team installs where users mostly
# stay on the same DC ; the cross-DC failover is the operational story
# (one host can die without taking the service down), not yet the data
# story.
#
# Image : `ghcr.io/openweft/weft-loom-server:v0.2.6` (current published
# tag). Override via the `image` input when chasing v0.3-rc1.
#
# Operator pre-flight (see docs/catalogue/loom-ha.md when written) :
#   1. Pick a domain (sets ROOT_URL + redirect URI for OIDC).
#   2. weft plugin install loom-ha \
#        --project devtools \
#        --input domain=loom.example.com \
#        --input dex_issuer=https://dex.example.com/dex \
#        --input dex_client_id=weft-loom
#
# Single-binary clean shutdown ; respawn via the platform's V0.1.10
# SchedulingRule (deployment.type=ha) takes over within
# WEFT_ZOMBIE_GC_CI_GRACE if a host crashes — set up automatically by
# the plugin's `respawn` block below.

plugin "loom-ha" {
  version     = "v1"
  kind        = "collaborative-editor"
  description = "Three weft-loom-server replicas behind Caddy with /api/healthz active-probes, one per DC. SSO via dex OIDC, per-replica local project store ; V0.3 will move the store to weft-block + S3."
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "ghcr.io/openweft/weft-loom-server:v0.2.6"
    help    = "Loom OCI image. BSD-3-Clause — openweft-owned."
  }

  input "domain" {
    type     = "string"
    required = true
    help     = "Public hostname loom serves (drives ROOT_URL + the OIDC redirect URI). Must resolve to the Caddy edge listener."
  }

  input "dex_issuer" {
    type    = "string"
    default = ""
    help    = "dex (or any OIDC IdP) issuer URL. Empty = use the V0.2 StaticVerifier (dev only — every WebSocket connection authenticates as the same hard-coded user). MUST be set in production."
  }

  input "dex_client_id" {
    type    = "string"
    default = "weft-loom"
    help    = "OIDC client_id loom uses to validate Bearer tokens from the web app."
  }

  input "data_volume_gib" {
    type    = "int"
    default = "20"
    help    = "Per-replica persistent volume for /var/lib/weft-loom (project file tree, compile artifacts). NOT replicated across DCs in V0.2 — that's V0.3 (weft-block + S3)."
  }

  input "tenant_network_cidrs" {
    type    = "string"
    default = "10.0.0.0/8"
    help    = "CIDR allowed to reach 8080/tcp. Narrow for production ; the Caddy edge listener sits inside it."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "loom" {
    cidr = "10.56.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security group
  # -----------------------------------------------------------------

  security_group "loom-app" {
    description = "weft-loom — 8080/tcp (HTTP + WebSocket) from the Caddy edge ; egress to dex + DNS."
    networks    = ["loom"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8080
      port_max    = 8080
      remote_cidr = "10.0.0.0/8"
      description = "Loom HTTP + WebSocket from the Caddy edge listener. The /api/healthz path on the same port is what the L7 active-probe targets."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "Outbound HTTPS for OIDC discovery + JWKs fetch from dex."
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
  # VMs — three replicas behind Caddy. No leader election ; rooms are
  # per-replica in-memory in V0.2 (V0.3 will add cross-DC sync via
  # weft-block + S3). The respawn block + deployment.type=ha label
  # wire in the V0.1.10 cross-host claim if a host dies.
  # -----------------------------------------------------------------

  vm "loom" {
    image    = "ghcr.io/openweft/weft-loom-server:latest"
    runtime  = "microvm"
    replicas = 3
    cpu      = 2
    mem_mb   = 2048
    disk_gb  = 5
    network  = "loom"

    placement {
      az   = "different"
      host = "different"
    }

    labels = {
      "deployment.type" = "ha"
      "role"            = "loom"
    }

    env_from "domain"        { env_name = "WEFT_LOOM_DOMAIN" }
    env_from "dex_issuer"    { env_name = "WEFT_LOOM_OIDC_ISSUER" }
    env_from "dex_client_id" { env_name = "WEFT_LOOM_OIDC_CLIENT_ID" }

    volume "data" {
      size_gib = 20
      format   = "raw"
      mount    = "/var/lib/weft-loom"
    }
  }

  # Post-install (not yet expressible in the plugin schema — run by hand
  # OR via the operator's bring-up script ; a future schema bump will
  # fold these in) :
  #
  #   weft scheduling-rule create \
  #     --name loom-ha \
  #     --selector "deployment.type=ha,role=loom" \
  #     --target-count 3 --anti-affinity host \
  #     --respawn-enabled --respawn-grace-period 5s \
  #     --respawn-max-restarts 5 --respawn-window 5m
  #
  # The Caddy L7 edge listener for {{input.domain}} is set up the same
  # way the rest of the platform's domains are (`weft router exposed-name
  # add` against the loom VM trio) — out of scope for this manifest
  # until the catalogue grows an `edge_listener` block primitive.
}
