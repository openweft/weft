# Caddy edge — a 3-replica north-south L7 proxy farm. Each replica
# pulls its config from a remote URL (Caddyfile or JSON) at boot and
# polls it for changes ; ACME terminates TLS via Let's Encrypt by
# default.
#
# Distinct from the cluster-internal weft-agent reverse proxy (see
# project_reverse_proxy_caddy). That plane is embedded in weft-agent
# and handles intra-cluster routing. THIS plugin is for ops who want
# a separate external-facing farm fronting the cluster from outside,
# typically behind a cloud LB or anycast IP.
#
# Image : `ghcr.io/openweft/weft-proxy:v0.1.0` (already published —
# same binary the agent embeds, but launched in standalone mode).
#
# Operator pre-flight (see docs/catalogue/caddy-edge.md):
#   1. Host the Caddyfile / JSON config somewhere the edge VMs can
#      fetch (S3 + presigned URL, internal versitygw bucket, raw GitHub
#      blob — anything HTTPS).
#   2. Point your cloud LB / DNS / anycast IP at the three replica
#      addresses (returned by `weft plugin status caddy-edge`).
#   3. weft plugin install caddy-edge \
#        --project edge \
#        --input caddy_config_url=https://s3.example.com/caddy.json \
#        --input acme_email=ops@example.com

plugin "caddy-edge" {
  version     = "v1"
  kind        = "edge-proxy"
  description = "Three Caddy replicas at network edge for north-south L7 ingress, ACME-managed TLS"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "ghcr.io/openweft/weft-proxy:v0.1.0"
    help    = "Caddy build the edge runs. Same image the in-cluster proxy uses, launched in standalone mode."
  }

  input "caddy_config_url" {
    type     = "string"
    required = true
    help     = "HTTPS URL of the Caddyfile or JSON config. Each replica fetches + polls this on a 30s interval (Caddy's `load` provisioner)."
  }

  input "acme_email" {
    type     = "string"
    required = true
    help     = "Email Let's Encrypt uses for cert expiry warnings. Don't reuse — this address sees every issuance event."
  }

  input "listen_https" {
    type    = "int"
    default = "443"
    help    = "Public HTTPS port the edge listens on. Override for unprivileged-port deploys (8443)."
  }

  input "listen_http" {
    type    = "int"
    default = "80"
    help    = "Public HTTP port — used for HTTP→HTTPS redirects and ACME HTTP-01 challenges."
  }

  input "config_poll_seconds" {
    type    = "int"
    default = "30"
    help    = "How often each replica re-fetches caddy_config_url. Lower = faster config rollout, more upstream traffic."
  }

  input "trusted_proxies_cidr" {
    type    = "string"
    default = "0.0.0.0/0"
    help    = "CIDR of upstream LBs Caddy should trust X-Forwarded-For from. Default trusts everything — pin to your LB subnet in production."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "edge" {
    cidr = "10.54.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "caddy-edge-public" {
    description = "Caddy edge — 80+443/tcp from the world, egress to upstreams + config source + Let's Encrypt + DNS."
    networks    = ["edge"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 80
      port_max    = 80
      remote_cidr = "0.0.0.0/0"
      description = "Public HTTP for ACME HTTP-01 + redirect to HTTPS."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "Public HTTPS."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "HTTPS upstreams + Let's Encrypt directory + config_poll fetch."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 80
      port_max    = 80
      remote_cidr = "0.0.0.0/0"
      description = "HTTP upstreams (rare) + plaintext config fetches."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 8080
      port_max    = 8080
      remote_cidr = "10.0.0.0/8"
      description = "Common backend upstream port across tenant networks."
    }

    rule "egress" {
      protocol    = "udp"
      port_min    = 53
      port_max    = 53
      remote_cidr = "0.0.0.0/0"
      description = "DNS — name resolution for upstreams + Let's Encrypt."
    }
  }

  # -----------------------------------------------------------------
  # VMs
  # -----------------------------------------------------------------

  vm "caddy" {
    image    = "ghcr.io/openweft/weft-proxy:v0.1.0"
    replicas = 3
    cpu      = 2
    mem_mb   = 2048
    disk_gb  = 10
    network  = "edge"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "caddy_config_url"     { env_name = "CADDY_CONFIG_URL" }
    env_from "acme_email"           { env_name = "CADDY_ACME_EMAIL" }
    env_from "listen_https"         { env_name = "CADDY_LISTEN_HTTPS" }
    env_from "listen_http"          { env_name = "CADDY_LISTEN_HTTP" }
    env_from "config_poll_seconds"  { env_name = "CADDY_CONFIG_POLL_SECONDS" }
    env_from "trusted_proxies_cidr" { env_name = "CADDY_TRUSTED_PROXIES_CIDR" }

    # Persistent ACME cert + key cache — losing this means every
    # replica re-issues from Let's Encrypt at restart, easy way to
    # hit rate limits.
    volume "certs" {
      size_gib = 2
      format   = "raw"
      mount    = "/data/caddy"
    }
  }
}
