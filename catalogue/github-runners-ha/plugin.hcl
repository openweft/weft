# GitHub Actions runners — HA 3-DC layout.
#
# Three self-hosted GitHub Actions runner replicas pinned one per AZ.
# Each replica registers against the operator-supplied org or repo via
# a short-lived registration token (the agent rotates it every hour
# using the github_pat scope `repo,workflow`).
#
# Operator pre-flight (see docs/catalogue/github-runners-ha.md):
#   1. Settings → Actions → Runners → "New self-hosted runner"
#      (org-wide: org/<org>/settings/actions/runners)
#   2. Copy the registration token *or* mint a PAT with admin:org
#      scope so the runner-side helper can mint tokens on its own.
#   3. weft plugin install github-runners-ha \
#        --project ci \
#        --input github_pat=ghp_xxx \
#        --input github_url=https://github.com/openweft

plugin "github-runners-ha" {
  version     = "v1"
  kind        = "runner-farm"
  description = "Three GitHub Actions runner replicas with hard anti-affinity across DCs"
  layout      = "ha-3dc"

  input "github_pat" {
    type     = "string"
    required = true
    secret   = true
    help     = "GitHub PAT with admin:org (org runners) or repo:admin (repo runners)"
  }

  input "github_url" {
    type     = "string"
    required = true
    help     = "Org or repo URL the runners attach to (https://github.com/<org> or https://github.com/<org>/<repo>)"
  }

  input "labels" {
    type    = "string"
    default = "weft,self-hosted,linux,x64"
    help    = "Comma-separated runner labels"
  }

  input "replicas" {
    type    = "int"
    default = "3"
    help    = "Number of runner replicas (default 3, one per DC)"
  }

  network "runners" {
    cidr = "10.43.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  security_group "runners-egress" {
    description = "GitHub Actions runners — outbound to github.com API, no inbound"
    networks    = ["runners"]

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "HTTPS to api.github.com and github.com"
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 22
      port_max    = 22
      remote_cidr = "0.0.0.0/0"
      description = "git+ssh fetch for actions/checkout against private repos"
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
    image    = "ghcr.io/openweft/weft-runner-github:v0.1.0"
    replicas = 3
    cpu      = 2
    mem_mb   = 4096
    disk_gb  = 20
    network  = "runners"

    placement {
      az = "different"
    }

    env_from "github_pat" {
      env_name = "GH_RUNNER_TOKEN"
    }

    env_from "github_url" {
      env_name = "GH_RUNNER_URL"
    }

    env_from "labels" {
      env_name = "GH_RUNNER_LABELS"
    }

    volume "cache" {
      size_gib = 10
      format   = "raw"
    }
  }
}
