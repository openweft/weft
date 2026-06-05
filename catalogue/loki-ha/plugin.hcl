# Loki HA — three Loki replicas in simple-scalable-mode, one per DC.
#
# Loki HA needs object storage for the chunk + index store, so this
# plugin is BYO-S3 : point it at versitygw (catalogue/versitygw-ha gives you
# a local one), Wasabi, R2, AWS S3, or any S3-compatible endpoint.
# Each replica runs the all-in-one `loki -target=all` binary (also
# called "simple scalable" when scaled out behind a load balancer) —
# we trade the microservices-mode flexibility for a 3-VM fleet that
# tops out around 1 TB/day per replica. If you need more, split into
# read/write targets and add a separate compactor.
#
# Image : upstream `grafana/loki:3.3` by default. No openweft fork.
#
# Operator pre-flight (see docs/catalogue/loki-ha.md):
#   1. Provision an S3 bucket (versitygw works) + access/secret keys.
#   2. weft plugin install loki-ha \
#        --project observability \
#        --input s3_endpoint=http://minio.weft:9000 \
#        --input s3_bucket=loki-chunks \
#        --input s3_access_key=$LOKI_AK \
#        --input s3_secret_key=$LOKI_SK
#
# NOTE on `replication_factor` : the schema accepts an integer literal
# or `input.<name>` only on the `count` attribute, not on the static
# `replicas` int. The VM count is pinned at 3 (one-per-DC = the whole
# point of the HA layout) and `replication_factor` is fed into Loki's
# own config via env_from. Operators who want fewer replicas can clone
# the manifest ; setting replication_factor > replicas breaks Loki
# (it tries to write to more ingesters than exist).

plugin "loki-ha" {
  version     = "v1"
  kind        = "logs"
  description = "Three Loki replicas in simple-scalable-mode, one per DC, with S3 chunk + index storage"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "grafana/loki:3.3"
    help    = "Loki OCI image. Default tracks upstream grafana/loki ; no openweft fork."
  }

  input "retention_days" {
    type    = "int"
    default = "30"
    help    = "Compactor retention window in days. Older chunks are deleted from S3 on the next compaction cycle."
  }

  input "replication_factor" {
    type    = "int"
    default = "3"
    help    = "Loki ingester replication factor. MUST be ≤ the VM replica count (pinned at 3). Setting >3 breaks the write path."
  }

  input "s3_endpoint" {
    type     = "string"
    required = true
    help     = "S3-compatible endpoint URL (e.g. http://minio.weft:9000, https://s3.us-east-1.amazonaws.com)."
  }

  input "s3_bucket" {
    type     = "string"
    required = true
    help     = "Bucket holding chunks + indices. Create it ahead of install ; Loki won't auto-provision."
  }

  input "s3_access_key" {
    type     = "string"
    required = true
    secret   = true
    help     = "S3 access key id. Cluster secret store, never plain HCL."
  }

  input "s3_secret_key" {
    type     = "string"
    required = true
    secret   = true
    help     = "S3 secret access key. Cluster secret store, never plain HCL."
  }

  input "s3_region" {
    type    = "string"
    default = "weft-1"
    help    = "S3 region label. Sent in SigV4 signing ; arbitrary value works for versitygw."
  }

  input "cache_volume_gib" {
    type    = "int"
    default = "50"
    help    = "Per-replica boltdb-shipper compactor cache, mounted at /var/loki. Larger = fewer S3 round-trips on queries."
  }

  input "tenant_network_cidrs" {
    type    = "string"
    default = "10.0.0.0/8"
    help    = "Comma-free CIDR (single string) of tenant subnets allowed to push to 3100/tcp. Default opens RFC1918 10/8."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "logs" {
    cidr = "10.61.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "loki-logs" {
    description = "Loki replicas — 3100/tcp from tenants, 7946/tcp memberlist gossip between replicas, S3 egress."
    networks    = ["logs"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 3100
      port_max    = 3100
      remote_cidr = "10.0.0.0/8"
      description = "Loki HTTP API (push + query) from tenant Promtail / Vector / Grafana. Narrow via tenant_network_cidrs."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 7946
      port_max    = 7946
      remote_cidr = "10.61.0.0/24"
      description = "Memberlist gossip — ring state replication between Loki ingesters."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 9095
      port_max    = 9095
      remote_cidr = "10.61.0.0/24"
      description = "Inter-component gRPC — distributor → ingester writes inside the cluster."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 7946
      port_max    = 7946
      remote_cidr = "10.61.0.0/24"
      description = "Outbound side of the memberlist gossip mesh."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 9095
      port_max    = 9095
      remote_cidr = "10.61.0.0/24"
      description = "Outbound side of the inter-component gRPC mesh."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "HTTPS egress to S3 (chunks + indices)."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 9000
      port_max    = 9000
      remote_cidr = "10.0.0.0/8"
      description = "Plain S3 endpoint (versitygw on-cluster) — drop in production if all S3 traffic is HTTPS."
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
  # VMs — three Loki replicas. `-target=all` keeps the binary
  # one-process-per-VM ; scale out by adding more replicas if you
  # outgrow ~1 TB/day per node.
  # -----------------------------------------------------------------

  vm "loki" {
    image    = "grafana/loki:3.3"
    replicas = 3
    cpu      = 4
    mem_mb   = 8192
    disk_gb  = 20
    network  = "logs"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "retention_days"     { env_name = "LOKI_RETENTION_DAYS" }
    env_from "replication_factor" { env_name = "LOKI_REPLICATION_FACTOR" }
    env_from "s3_endpoint"        { env_name = "LOKI_S3_ENDPOINT" }
    env_from "s3_bucket"          { env_name = "LOKI_S3_BUCKET" }
    env_from "s3_access_key"      { env_name = "LOKI_S3_ACCESS_KEY" }
    env_from "s3_secret_key"      { env_name = "LOKI_S3_SECRET_KEY" }
    env_from "s3_region"          { env_name = "LOKI_S3_REGION" }

    volume "cache" {
      size_gib = 50
      format   = "raw"
      mount    = "/var/loki"
    }
  }
}
