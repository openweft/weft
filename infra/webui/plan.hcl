# weft infra plan — weft-webui dashboard
#
# 1 VM per DC, 3 replicas total, each serving the Svelte SPA + huma
# API surface. The binary is self-contained (//go:embed all:web/dist)
# so no external assets to fetch at runtime ; the only state the
# webui touches lives in etcd (via the wired weft-agent) and in
# operator JSON snapshots under /var/lib/weft-webui/.
#
# HA model is identical to the existing dex/zot pattern : 3 stateless
# replicas behind anycast-style static IPs ; the L7 Caddy plane (when
# wired) routes external traffic via the public Endpoint, and any
# CLI-served HTTP comes through the same weft-agent socket so the
# dashboard sees the same source-of-truth as `weft` itself.
#
# Bootstrap order (per the openweft pull-model + infra-in-microvms) :
#   file-weft → etcd → coredns → dex → zot → nats → **webui**
#
# webui lands LAST in the infra DAG because it depends on the agent
# being up — the binary calls weft-agent on /api/* via wclient.Dial
# at boot. Earlier services don't need webui ; only operators do.

service "webui" {
  description = "weft-webui — Svelte SPA + huma API dashboard, 3 stateless replicas behind the L7 Caddy plane"
  oci_image   = "ghcr.io/openweft/weft-webui:v0.2.0"

  resources {
    cpu_count  = 1
    memory_mib = 256
  }

  # No persistent volume — the embedded SPA assets ship inside the
  # binary, and inventory / audit / federation state all live in
  # etcd through the wired weft-agent. The webui's only on-disk
  # writes are JSON snapshots under /var/lib/weft-webui (audit
  # ring buffer, state-file history) — small enough that a fresh
  # microVM boots clean every time.

  # Tenant-facing subnet — operators reach the dashboard over the
  # L7 Caddy plane that listens on these static IPs.
  network {
    name      = "tenant-services"
    static_ip = ["10.255.2.40", "10.255.2.41", "10.255.2.42"]
  }

  cmdline = "weft.rootfs=virtiofs:rootfs0 weft.config=virtiofs:cfg"

  # The webui reads its listener config + the weft-agent socket
  # path from /etc/weft-webui/config.json. The Caddy edge plane
  # terminates TLS ; the webui itself stays plain HTTP behind it.
  config_file {
    path     = "/etc/weft-webui/config.json"
    template = <<-EOT
      {
        "user_listen": "0.0.0.0:8080",
        "admin_listen": "0.0.0.0:8088",
        "agent_socket": "/var/run/weft/weft.sock",
        "oidc": {
          "issuer": "https://dex.$BASE_DOMAIN",
          "client_id": "weft-webui",
          "redirect_url": "https://dashboard.$BASE_DOMAIN/oidc/callback"
        },
        "audit_dir": "/var/lib/weft-webui/audit",
        "state_dir": "/var/lib/weft-webui/state"
      }
    EOT
  }

  # webui needs the agent + dex tokens ; both are in the DAG above.
  depends_on = ["etcd", "dex"]

  health {
    type   = "http"
    cmd    = "http://$VM_IP:8080/api/readyz"
    period = "10s"
  }
}
