# Prometheus HA — three federated Prometheus replicas, one per DC.
#
# Each replica scrapes the same target set independently and stamps
# its samples with the cluster-wide `external_labels.cluster` plus a
# per-replica `replica` label. Federation queries (or a remote-write
# target like Cortex/Mimir/Thanos) deduplicate on the (cluster, job,
# instance) tuple and ignore the replica axis.
#
# This is the "stop running a single Prometheus on a pet VM" plugin :
# you get DC-failure-tolerant scraping out of the box, with the same
# upstream `prom/prometheus` image and the same scrape config you
# already author by hand.
#
# Image : upstream `prom/prometheus:v2.55` by default. An openweft-
# built `ghcr.io/openweft/prometheus-ha:v0.1.0` is planned but NOT
# YET PUBLISHED ; switch the `image` input once it lands.
#
# Operator pre-flight (see docs/catalogue/prometheus-ha.md):
#   1. Decide on a cluster name (becomes external_labels.cluster).
#   2. Optional : pre-create a remote_write sink (Mimir / VictoriaMetrics).
#   3. weft plugin install prometheus-ha \
#        --project observability \
#        --input external_labels_cluster=prod-eu-west-1 \
#        --input retention=15d

plugin "prometheus-ha" {
  version     = "v1"
  kind        = "metrics"
  description = "Three federated Prometheus replicas, one per DC, with TSDB persistence and optional remote_write"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "prom/prometheus:v2.55"
    help    = "Prometheus OCI image. Default tracks upstream until ghcr.io/openweft/prometheus-ha:v0.1.0 is published."
  }

  input "external_labels_cluster" {
    type     = "string"
    required = true
    help     = "Cluster label stamped onto every sample (e.g. prod-eu-west-1). Federation/remote-write keys on this to deduplicate."
  }

  input "scrape_interval" {
    type    = "string"
    default = "30s"
    help    = "Global scrape interval. 30s is a sensible default ; drop to 15s for kubelet-style cardinality, raise to 60s for cost-sensitive deploys."
  }

  input "retention" {
    type    = "string"
    default = "15d"
    help    = "Local TSDB retention (--storage.tsdb.retention.time). Anything older is dropped ; use remote_write for longer history."
  }

  input "remote_write_url" {
    type    = "string"
    default = ""
    help    = "Optional remote_write endpoint (Mimir / Cortex / VictoriaMetrics). Empty disables remote_write."
  }

  input "tsdb_volume_gib" {
    type    = "int"
    default = "200"
    help    = "Per-replica TSDB volume mounted at /prometheus. Sizing : ~1.5 bytes/sample × samples/sec × retention seconds."
  }

  input "tenant_network_cidrs" {
    type    = "string"
    default = "10.0.0.0/8"
    help    = "Comma-free CIDR (single string) of operator subnets allowed to reach 9090/tcp. Default opens RFC1918 10/8 — narrow for production."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "metrics" {
    cidr = "10.60.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "prometheus-metrics" {
    description = "Prometheus replicas — 9090/tcp from operator subnets, scrape egress, optional remote_write egress."
    networks    = ["metrics"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 9090
      port_max    = 9090
      remote_cidr = "10.0.0.0/8"
      description = "Prometheus UI + federation endpoint (/federate) from operator subnets. Narrow via tenant_network_cidrs input."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 9090
      port_max    = 9090
      remote_cidr = "10.60.0.0/24"
      description = "Inter-replica federation — each replica can /federate from its peers for cross-DC alert resilience."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 1
      port_max    = 65535
      remote_cidr = "10.0.0.0/8"
      description = "Outbound scrapes against tenant exporters (node_exporter, application /metrics)."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "HTTPS egress for remote_write sinks (Mimir / Grafana Cloud / VictoriaMetrics)."
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
  # VMs — three Prometheus replicas. Each scrapes the same targets
  # independently ; the federated dedup happens downstream (Grafana
  # query layer or remote_write receiver).
  # -----------------------------------------------------------------

  vm "prometheus" {
    image    = "prom/prometheus:v2.55"
    runtime  = "microvm"
    replicas = 3
    cpu      = 4
    mem_mb   = 8192
    disk_gb  = 20
    network  = "metrics"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "external_labels_cluster" { env_name = "PROMETHEUS_EXTERNAL_CLUSTER" }
    env_from "scrape_interval"         { env_name = "PROMETHEUS_SCRAPE_INTERVAL" }
    env_from "retention"               { env_name = "PROMETHEUS_RETENTION" }
    env_from "remote_write_url"        { env_name = "PROMETHEUS_REMOTE_WRITE_URL" }

    volume "tsdb" {
      size_gib = 200
      format   = "raw"
      mount    = "/prometheus"
    }
  }
}
