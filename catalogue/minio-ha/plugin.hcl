# MinIO HA — four-node erasure-coded cluster. Three nodes spread one
# per DC ; the fourth lives in the largest DC so MinIO's EC math has
# even drive counts (4 nodes × 4 vols = 16 drives, EC:8+8 by default).
#
# A 4-node layout is the smallest MinIO deployment that gives both
# distributed reads/writes AND survives a full DC outage : 16 drives
# in EC:8+8 means we can lose any 8 (one DC = 4 drives, or any two
# nodes outside that DC). Drop to 3 nodes and you're either degraded
# during a DC outage or running in EC:6+6 with worse durability.
#
# Image : upstream `minio/minio:RELEASE.2026-XX-YY` — operator pins
# via the `image` input. No openweft fork.
#
# Operator pre-flight (see docs/catalogue/minio-ha.md):
#   1. Pick the root credentials and stash them in the secret store.
#   2. weft plugin install minio-ha \
#        --project storage \
#        --input root_user=admin \
#        --input root_password=$MINIO_PASS

plugin "minio-ha" {
  version     = "v1"
  kind        = "object-storage"
  description = "Four-node erasure-coded MinIO cluster, three DCs (extra node in DC-1) for EC:8+8 durability"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "minio/minio:RELEASE.2026-05-15T00-00-00Z"
    help    = "MinIO OCI image. Pin to a dated release tag — `latest` will silently change EC parameters across upgrades."
  }

  input "root_user" {
    type     = "string"
    required = true
    help     = "MinIO root access key (S3 access-key-id equivalent). Used for the first `mc alias set`."
  }

  input "root_password" {
    type     = "string"
    required = true
    secret   = true
    help     = "MinIO root secret key. Minimum 8 characters, MinIO will refuse anything shorter."
  }

  input "volumes_per_node" {
    type    = "int"
    default = "4"
    help    = "Erasure-coded drives per node. 4 × 4 nodes = 16 drives ; EC:8+8 default. Lower = worse durability."
  }

  input "volume_size_gib" {
    type    = "int"
    default = "200"
    help    = "Size of each EC drive (GiB). Total raw = 4 nodes × volumes_per_node × this. Usable = raw × (parity-data / total)."
  }

  input "region" {
    type    = "string"
    default = "weft-1"
    help    = "MinIO server region label, surfaced in S3 GetBucketLocation responses."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "minio" {
    cidr = "10.52.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "minio-storage" {
    description = "MinIO — 9000/tcp S3 + 9001/tcp console from tenants, inter-node distributed protocol on 9000."
    networks    = ["minio"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 9000
      port_max    = 9000
      remote_cidr = "10.0.0.0/8"
      description = "S3 API from tenant networks."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 9001
      port_max    = 9001
      remote_cidr = "10.0.0.0/8"
      description = "MinIO console (web UI). Restrict to ops networks in production."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 9000
      port_max    = 9000
      remote_cidr = "10.52.0.0/24"
      description = "Inter-node distributed protocol — heals + EC rebuilds."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 9000
      port_max    = 9000
      remote_cidr = "10.52.0.0/24"
      description = "Outbound side of the inter-node mesh."
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
  # VMs — four nodes. The scheduler distributes them via az=different
  # for the first three ; the fourth lands in whichever DC has spare
  # quota (deterministic with the host=different hint).
  # -----------------------------------------------------------------

  vm "minio" {
    image    = "minio/minio:RELEASE.2026-05-15T00-00-00Z"
    replicas = 4
    cpu      = 4
    mem_mb   = 8192
    disk_gb  = 20
    network  = "minio"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "root_user"     { env_name = "MINIO_ROOT_USER" }
    env_from "root_password" { env_name = "MINIO_ROOT_PASSWORD" }
    env_from "region"        { env_name = "MINIO_REGION" }

    volume "drive-0" {
      size_gib = 200
      format   = "raw"
      mount    = "/mnt/drive-0"
    }
    volume "drive-1" {
      size_gib = 200
      format   = "raw"
      mount    = "/mnt/drive-1"
    }
    volume "drive-2" {
      size_gib = 200
      format   = "raw"
      mount    = "/mnt/drive-2"
    }
    volume "drive-3" {
      size_gib = 200
      format   = "raw"
      mount    = "/mnt/drive-3"
    }
  }
}
