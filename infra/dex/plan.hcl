# vzd infra plan — dex OIDC identity provider
#
# 1 VM per DC, all 3 backed by the same etcd cluster (storage
# backend "etcd"). HA via a small load balancer in front; clients
# get whichever pod is closest.

service "dex" {
  description = "OIDC identity provider — issues tokens for vzd/ncl/vzc/zot"
  oci_image   = "ghcr.io/dexidp/dex:v2.40.0"

  resources {
    cpu_count  = 1
    memory_mib = 256
  }

  # Storage : etcd, co-tenants the platform control-plane cluster.
  # No dedicated persistent volume — sessions, refresh tokens, and
  # static client configs all live in etcd keys. No `volume` block
  # by design : the `[]VolumeRef` decoder treats absence as empty.

  # Tenant-facing subnet — both vzd / zot / vzc need to reach dex
  # to validate bearer tokens.
  network {
    name      = "tenant-services"
    static_ip = ["10.255.2.20", "10.255.2.21", "10.255.2.22"]
  }

  cmdline = "ncl.rootfs=virtiofs:rootfs0 ncl.config=virtiofs:cfg"

  # Dex's config.yaml — generated per VM. The OCI image's
  # entrypoint reads this from /etc/dex/config.yaml.
  config_file {
    path     = "/etc/dex/config.yaml"
    template = <<-EOT
      issuer: https://dex.$BASE_DOMAIN
      storage:
        type: etcd
        config:
          endpoints:
            - https://10.255.1.10:2379
            - https://10.255.1.11:2379
            - https://10.255.1.12:2379
          ssl:
            caFile:   /etc/dex/etcd-tls/ca.crt
            certFile: /etc/dex/etcd-tls/client.crt
            keyFile:  /etc/dex/etcd-tls/client.key

      web:
        https: 0.0.0.0:5556
        tlsCert: /etc/dex/tls/server.crt
        tlsKey:  /etc/dex/tls/server.key

      # Bootstrap-mode static admin. Replace with upstream
      # federation after the platform is self-hosted.
      staticPasswords:
        - email: "admin@$BASE_DOMAIN"
          hash:  "$ADMIN_BCRYPT_HASH"
          username: admin
          userID:  "00000000-0000-0000-0000-000000000001"

      # Client registrations for the platform's own consumers.
      staticClients:
        - id: vzd-api
          name: vzd API server
          redirectURIs:
            - https://vzd.$BASE_DOMAIN/oidc/callback
          secret: '$VZD_CLIENT_SECRET'
        - id: zot-registry
          name: zot OCI registry
          redirectURIs:
            - https://zot.$BASE_DOMAIN/auth/oidc/callback
          secret: '$ZOT_CLIENT_SECRET'
        - id: vzc-cli
          name: vzc CLI (device-grant)
          public: true
          # vzc uses the device-grant flow — no client secret.
    EOT
  }

  depends_on = ["etcd"]

  health {
    type   = "http"
    cmd    = "https://$VM_IP:5556/healthz"
    period = "5s"
  }
}
