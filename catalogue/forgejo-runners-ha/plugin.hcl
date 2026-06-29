# Forgejo runners — HA 3-DC layout.
#
# Three forgejo-runner replicas with hard anti-affinity across DCs.
# Forgejo's `act_runner` registers against the operator-supplied
# Forgejo instance URL with a one-shot token from the admin UI.
#
# Operator pre-flight (see docs/catalogue/forgejo-runners-ha.md):
#   1. Site Administration → Actions → Runners → "Create new runner"
#      (or per-org via /<org>/-/settings/actions/runners)
#   2. Copy the one-time registration token.
#   3. weft plugin install forgejo-runners-ha \
#        --project ci \
#        --input registration_token=<token> \
#        --input forgejo_url=https://code.example.org

plugin "forgejo-runners-ha" {
  version     = "v1"
  kind        = "runner-farm"
  description = "Three Forgejo (act_runner) replicas with hard anti-affinity across DCs"
  layout      = "ha-3dc"

  input "registration_token" {
    type     = "string"
    required = true
    secret   = true
    help     = "Forgejo runner one-shot registration token"
  }

  input "forgejo_url" {
    type     = "string"
    required = true
    help     = "Forgejo instance URL (e.g. https://code.example.org)"
  }

  input "labels" {
    type    = "string"
    default = "weft:docker://node:20-bullseye"
    help    = "Comma-separated act_runner labels (label:executor format)"
  }

  input "replicas" {
    type    = "int"
    default = "3"
    help    = "Number of runner replicas (default 3, one per DC)"
  }

  network "runners" {
    cidr = "10.44.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  security_group "runners-egress" {
    description = "Forgejo runners — outbound to the Forgejo instance, no inbound"
    networks    = ["runners"]

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "HTTPS to the Forgejo instance API"
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 22
      port_max    = 22
      remote_cidr = "0.0.0.0/0"
      description = "git+ssh fetch for ssh:// remotes"
    }

    rule "egress" {
      protocol    = "udp"
      port_min    = 53
      port_max    = 53
      remote_cidr = "0.0.0.0/0"
      description = "DNS"
    }
  }

  vm "runner" {
    image    = "ghcr.io/openweft/weft-runner-forgejo:v0.1.0"
    runtime  = "microvm"
    replicas = 3
    cpu      = 2
    mem_mb   = 4096
    disk_gb  = 20
    network  = "runners"

    placement {
      az = "different"
    }

    env_from "registration_token" {
      env_name = "FORGEJO_RUNNER_TOKEN"
    }

    env_from "forgejo_url" {
      env_name = "FORGEJO_URL"
    }

    env_from "labels" {
      env_name = "FORGEJO_RUNNER_LABELS"
    }

    volume "cache" {
      size_gib = 10
      format   = "raw"
    }
  }
}
