# weft infra plan — iRODS data-management cluster
#
# Three micro-VMs, one per DC, running the weft-ha-irods agent. The
# pattern mirrors weft-ha-forgejo / weft-ha-postgresql per the
# project_catalogue_irods_forgejo memory : a stateless-replica HA
# stack coordinated through the cluster etcd DCS, with PostgreSQL
# (iCAT) backed by an underlying ha-postgresql plan (depends_on
# below) and per-resource storage volumes mounted on each VM.
#
# Storage nodes are the same micro-VMs — each one runs both the
# iRODS server and an iRODS "resource" pointing at its local
# volume mount. To grow capacity, add another DC to cluster.hcl :
# weft up emits a new place-replica step which lays down a 4th
# iRODS VM with its own resource. Geo-replication policies are
# configured operator-side via the iRODS rules engine (not in this
# plan — it's tenant-driven).

service "irods" {
  description = "3-DC iRODS data-management plane — federated collections + replicated resources"
  oci_image   = "ghcr.io/openweft/weft-ha-irods:v0.2.0-rc1"

  resources {
    cpu_count  = 2
    memory_mib = 2048
  }

  # Data volume per replica — each iRODS server's resource lives on
  # its own block volume so a host loss doesn't impact other DCs.
  # 1 TiB per DC is the default ; tune via `weft up --apply` after
  # editing cluster.hcl's storage_pool block.
  volume {
    mount    = "/var/lib/irods/Vault"
    uuid     = "irods-vault-dc1"
    size_gib = 1024
  }
  volume {
    mount    = "/var/lib/irods/Vault"
    uuid     = "irods-vault-dc2"
    size_gib = 1024
  }
  volume {
    mount    = "/var/lib/irods/Vault"
    uuid     = "irods-vault-dc3"
    size_gib = 1024
  }

  # iRODS-specific subnet — the iCAT + resource servers gossip on
  # 1247/tcp ; user clients reach them through the loadbalancer
  # on the tenant-services subnet (configured tenant-side).
  network {
    name      = "tenant-services"
    static_ip = ["10.255.2.50", "10.255.2.51", "10.255.2.52"]
  }

  cmdline = "weft.rootfs=virtiofs:rootfs0 weft.config=virtiofs:cfg"

  # weft-ha-irods agent reads /etc/irods/server_config.json at
  # boot ; the tokens marked $DC / $ZONE / $PEERS / $ICAT_HOST get
  # filled in at deploy time. iCAT_HOST is the floating-IP of the
  # ha-postgresql cluster (declared in plan.hcl of postgres-ha).
  config_file {
    path     = "/etc/irods/server_config.json"
    template = <<-EOT
      {
        "default_resource_name": "weft-resc-$DC",
        "zone_name": "$ZONE",
        "zone_port": 1247,
        "catalog_provider_hosts": ["$ICAT_HOST"],
        "catalog_service_role": "consumer",
        "icat_host": "$ICAT_HOST",
        "federation": [],
        "advanced_settings": {
          "default_number_of_transfer_threads": 4,
          "transfer_buffer_size_for_parallel_transfer_in_megabytes": 4,
          "transfer_chunk_size_for_parallel_transfer_in_megabytes": 40
        },
        "dcs_endpoints": "$PEERS"
      }
    EOT
  }

  # Depends on : etcd (DCS coordination) + dex (PAM/OIDC delegation).
  # The iCAT PostgreSQL backend is provided by the postgresql-ha
  # catalogue plugin which the operator installs separately ; it
  # isn't an infra-layer service. iCAT_HOST in the template resolves
  # to that plugin's service VIP at deploy time.
  depends_on = ["etcd", "dex"]

  health {
    type   = "exec"
    cmd    = "/var/lib/irods/scripts/irods-grid-status.sh"
    period = "10s"
  }
}
