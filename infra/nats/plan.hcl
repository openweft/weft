# vzd infra plan — NATS event bus
#
# Three micro-VMs, one per DC, joined in cluster mode (full mesh
# between the three peers). JetStream enabled for durable streams;
# the platform's PlatformEvent traffic flows on subject
# `vzd.events.>` and is retained for 24 h so a freshly-started vzc
# / ncl picks up the recent timeline.
#
# Bootstrap order ([[infra-in-micro-vms]]):
#   file-vzd → etcd → dex → zot → **nats** → self-promote
#
# NATS deploys *after* dex so JWT auth (via dex-issued tokens via
# the NATS JWT integration) is ready when the cluster comes up.
# Local dev still works without dex: an unauthenticated NATS in
# local-only mode is fine when there are no remote subscribers.

service "nats" {
  description = "NATS event bus — 3-DC cluster + JetStream for vzd PlatformEvents"
  oci_image   = "docker.io/nats:2.11-alpine"

  resources {
    cpu_count  = 1
    memory_mib = 512
  }

  # JetStream needs persistent storage : per-DC volume so a NATS-VM
  # restart preserves replicated state.  Sized for a week of event
  # traffic at typical platform scale ; bump as needed.
  volume {
    mount    = "/var/lib/nats/jetstream"
    uuid     = "nats-data-dc1"
    size_gib = 16
  }
  volume {
    mount    = "/var/lib/nats/jetstream"
    uuid     = "nats-data-dc2"
    size_gib = 16
  }
  volume {
    mount    = "/var/lib/nats/jetstream"
    uuid     = "nats-data-dc3"
    size_gib = 16
  }

  # Tenant-facing : vzd / vzc / ncl all open NATS clients here.
  # Cluster peer traffic (port 6222) is internal to this subnet.
  network {
    name      = "tenant-services"
    static_ip = ["10.255.3.30", "10.255.3.31", "10.255.3.32"]
  }

  # Standard ncl-init rootfs share + service config mounted at
  # /etc/nats/nats.conf .
  cmdline = "ncl.rootfs=virtiofs:rootfs0 ncl.config=virtiofs:cfg"

  # NATS server config rendered by vzd at deploy time.  Tokens
  # $REPLICA / $PEERS / $DC / $PRIVATE_IP filled in per VM.  See
  # README.md for the bootstrap detail.
  config_file {
    path     = "/etc/nats/nats.conf"
    template = <<-EOT
      server_name: "nats-$DC"
      listen:      "0.0.0.0:4222"
      http_port:   8222

      cluster {
        name:     "vzd-events"
        listen:   "0.0.0.0:6222"
        routes: [
          $PEERS
        ]
      }

      jetstream {
        store_dir: "/var/lib/nats/jetstream"
        max_memory_store:   256MB
        max_file_store:     16GB
      }

      # JWT authentication via dex.  The operator_jwt + system_user
      # are materialised at deploy time from dex's NATS integration
      # ; see infra/dex/plan.hcl for the dex-side configuration.
      operator: "/etc/nats/operator.jwt"
      resolver: MEMORY
      resolver_preload: {
        # Populated from dex on first boot ; placeholder here.
      }

      # Local-dev shortcut : when operator_jwt is empty the server
      # falls back to anonymous, suitable for single-host vzd.
      # Production deploys MUST set operator_jwt + cleartext: false.
      no_auth_user: "vzd"
      cleartext:    true
    EOT
  }

  # dex must be up first ; once JWT auth wires through, NATS rejects
  # tokens dex would not have issued.
  depends_on = ["dex"]

  # Readiness : the /healthz endpoint NATS exposes on the http_port.
  health {
    type   = "http"
    cmd    = "http://$VM_IP:8222/healthz"
    period = "5s"
  }

  # 3-DC HA cluster : one replica per AZ, and within each AZ
  # avoid colocating with anything else NATS-shaped. Rack also
  # set to "different" so we don't end up with two NATS replicas
  # behind the same ToR switch / PDU when the cluster grows
  # multi-rack inside one AZ.
  placement {
    count = 3
    az    = "different"
    rack  = "different"
    host  = "different"
  }
}
