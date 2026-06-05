# versitygw HA — three-node S3 gateway cluster spread one per DC.
#
# Replaces the previous minio-ha plugin (removed 2026-06 per the
# openweft no-AGPL policy ; see memory feedback_no_minio). versitygw
# (Versity Software, Apache-2.0) is a pure S3-protocol gateway over
# POSIX backends ; we front it with three replicas behind the L7
# Caddy in weft-agent so the operator gets a single VIP that hashes
# requests by bucket+key to a deterministic backend, with health-
# check failover when a replica drops.
#
# Why three (not four like the MinIO layout) : versitygw has no
# erasure-coding logic of its own — durability comes from the
# underlying volumes (replicated weft-block, or a CubeFS share).
# Three replicas survive a full DC outage and read scale linearly ;
# adding a fourth buys nothing.
#
# Operator pre-flight (see docs/catalogue/versitygw-ha.md):
#   1. Pick the root credentials and stash them in the secret store.
#   2. Decide on the backend : per-replica block volumes (default,
#      uses weft-block's replicated controllers) OR a shared CubeFS
#      share mounted into every replica (set backend=cubefs).
#   3. weft plugin install versitygw-ha \
#        --project storage \
#        --input root_access_key=AKIA... \
#        --input root_secret_key=$VW_SECRET

plugin "versitygw-ha" {
  version     = "v1"
  kind        = "object-storage"
  description = "Three-node versitygw (Apache-2.0) S3 gateway, one per DC, durability via weft-block-replicated volumes"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "ghcr.io/versity/versitygw:v1.0.13"
    help    = "versitygw OCI image. Pin to a dated release tag — `latest` may shift S3 conformance behaviour across upgrades."
  }

  input "root_access_key" {
    type     = "string"
    required = true
    help     = "Root S3 access-key-id. Used by the first `aws configure` to mint admin sub-keys."
  }

  input "root_secret_key" {
    type     = "string"
    required = true
    secret   = true
    help     = "Root S3 secret-access-key. Stash in the cluster secret store, never in plain HCL."
  }

  input "backend" {
    type    = "string"
    default = "block"
    help    = "`block` = per-replica weft-block volumes (cluster-managed replication) ; `cubefs` = shared CubeFS mount (requires a pre-existing share — set cubefs_share)."
  }

  input "cubefs_share" {
    type    = "string"
    default = ""
    help    = "CubeFS share name when backend=cubefs. Empty = use per-replica block volumes."
  }

  input "volumes_per_node" {
    type    = "int"
    default = "4"
    help    = "Block volumes per replica when backend=block. 4 × 3 nodes = 12 volumes ; versitygw treats each as a separate bucket-shard root."
  }

  input "volume_size_gib" {
    type    = "int"
    default = "200"
    help    = "Size of each block volume (GiB). Inert when backend=cubefs ; the CubeFS share manages its own quota."
  }

  input "region" {
    type    = "string"
    default = "weft-1"
    help    = "S3 region label. Arbitrary string ; clients pass it through unchanged."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "versitygw" {
    cidr = "10.52.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups — versitygw only needs the S3 port. There is no
  # inter-node distributed protocol the way MinIO has — replicas are
  # independent and routing is done by the L7 Caddy in weft-agent.
  # -----------------------------------------------------------------

  security_group "versitygw-storage" {
    description = "versitygw — 7070/tcp S3 from tenants ; no inter-node mesh."
    networks    = ["versitygw"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 7070
      port_max    = 7070
      remote_cidr = "10.0.0.0/8"
      description = "S3 API from tenant networks."
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
  # VMs — three replicas, anti-affinity at the AZ + host level.
  # -----------------------------------------------------------------

  vm "versitygw" {
    image    = "ghcr.io/versity/versitygw:v1.0.13"
    replicas = 3
    cpu      = 2
    mem_mb   = 4096
    disk_gb  = 20
    network  = "versitygw"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "root_access_key" { env_name = "VGW_ROOT_ACCESS_KEY" }
    env_from "root_secret_key" { env_name = "VGW_ROOT_SECRET_KEY" }
    env_from "region"          { env_name = "VGW_REGION" }

    # Per-replica block volumes when backend=block. CubeFS-backed
    # deployments leave this empty and mount the shared volume
    # through the share fan-out path instead — see the
    # `share_attach` block below.
    volume "drive" {
      size_gib = 200
      format   = "raw"
      mount    = "/data/drive"
      count    = input.volumes_per_node
    }

    # When the operator chose backend=cubefs, the same plugin attaches
    # the named share to every replica's /data ; the operator MUST
    # have created the share separately (CubeFS shares aren't auto-
    # created by this plugin).
    share_attach "cubefs" {
      share      = input.cubefs_share
      mount      = "/data/shared"
      enabled_if = "input.backend == \"cubefs\" && input.cubefs_share != \"\""
    }
  }
}
