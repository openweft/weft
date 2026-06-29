# GitLab runners — HA 3-DC layout.
#
# Three replicas of gitlab-runner spread across the three availability
# zones via a hard anti-affinity scheduling rule. Each replica gets a
# 10 GiB ephemeral cache volume and outbound-only access to gitlab.com
# (or the operator's self-hosted GitLab) through the dedicated
# `runners` network.
#
# Operator pre-flight (see docs/catalogue/gitlab-runners-ha.md):
#   1. Settings → CI/CD → Runners → "New project/group/instance runner"
#   2. Copy the registration token.
#   3. weft plugin install gitlab-runners-ha \
#        --project ci \
#        --input registration_token=glrt-xxx \
#        --input gitlab_url=https://gitlab.com

plugin "gitlab-runners-ha" {
  version     = "v1"
  kind        = "runner-farm"
  description = "Three GitLab CI runner replicas with hard anti-affinity across DCs"
  layout      = "ha-3dc"

  input "registration_token" {
    type     = "string"
    required = true
    secret   = true
    help     = "GitLab runner registration token (gitlab.com or self-hosted)"
  }

  input "gitlab_url" {
    type    = "string"
    default = "https://gitlab.com"
    help    = "GitLab instance URL the runners register against"
  }

  input "replicas" {
    type    = "int"
    default = "3"
    help    = "Number of runner replicas (default 3, one per DC)"
  }

  input "concurrency" {
    type    = "int"
    default = "4"
    help    = "Max concurrent jobs per runner"
  }

  network "runners" {
    cidr = "10.42.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  security_group "runners-egress" {
    description = "GitLab runners — outbound to gitlab.com / self-hosted GitLab, no inbound"
    networks    = ["runners"]

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "HTTPS to gitlab.com API and Container Registry"
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 22
      port_max    = 22
      remote_cidr = "0.0.0.0/0"
      description = "git+ssh fetch for projects using ssh:// remotes"
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
    image    = "ghcr.io/openweft/weft-runner-gitlab:v0.1.0"
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
      env_name = "GITLAB_RUNNER_TOKEN"
    }

    env_from "gitlab_url" {
      env_name = "GITLAB_URL"
    }

    env_from "concurrency" {
      env_name = "GITLAB_RUNNER_CONCURRENCY"
    }

    volume "cache" {
      size_gib = 10
      format   = "raw"
    }
  }
}
