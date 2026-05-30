# weft infra plan — zot OCI registry
#
# 1 VM per DC, sync-mirroring between them. Each VM uses S3-
# compatible object storage by default (matches the
# cloud_platform_direction.md "default to cloud-native"
# preference); a local-fs override is provided for single-host
# dev / air-gapped lab setups.

service "zot" {
  description = "OCI Distribution registry — kernel/initrd/plans + signed artefacts"
  oci_image   = "ghcr.io/project-zot/zot:v2.1.0"

  resources {
    cpu_count  = 1
    memory_mib = 512
  }

  # Local-fs is the dev path. In prod, set storage_backend = "s3"
  # in the deploy invocation and these volumes go unused.
  volume {
    mount    = "/var/lib/zot"
    uuid     = "zot-data-dc1"
    size_gib = 256
  }
  volume {
    mount    = "/var/lib/zot"
    uuid     = "zot-data-dc2"
    size_gib = 256
  }
  volume {
    mount    = "/var/lib/zot"
    uuid     = "zot-data-dc3"
    size_gib = 256
  }

  # Tenant-facing — every weft consumer + every developer pushes
  # / pulls from zot. The infra subnet doesn't need zot (etcd /
  # dex never speak OCI).
  network {
    name      = "tenant-services"
    static_ip = ["10.255.2.30", "10.255.2.31", "10.255.2.32"]
  }

  cmdline = "weft.rootfs=virtiofs:rootfs0 weft.config=virtiofs:cfg"

  # zot config — JSON because that's what zot reads. The HCL
  # plan around it stays HCL ; only this third-party file is
  # JSON (see [[hcl-over-json]] : JSON for append-only / third-
  # party configs).
  config_file {
    path     = "/etc/zot/config.json"
    template = <<-EOT
      {
        "distSpecVersion": "1.1.0",
        "storage": {
          "rootDirectory": "/var/lib/zot",
          "dedupe": true,
          "gc": true,
          "gcDelay": "1h",
          "gcInterval": "24h"
        },
        "http": {
          "address": "0.0.0.0",
          "port": "8080",
          "tls": {
            "cert": "/etc/zot/tls/server.crt",
            "key":  "/etc/zot/tls/server.key"
          },
          "auth": {
            "openID": {
              "providers": {
                "dex": {
                  "name":         "weft-dex",
                  "clientid":     "zot-registry",
                  "clientsecret": "$ZOT_CLIENT_SECRET",
                  "scopes":       ["openid", "email", "groups"],
                  "issuer":       "https://dex.$BASE_DOMAIN"
                }
              }
            }
          }
        },
        "extensions": {
          "sync": {
            "enable": true,
            "registries": [
              {
                "urls":     ["https://upstream-zot.$PEER_DC.$BASE_DOMAIN"],
                "onDemand": true,
                "tlsVerify": true
              }
            ]
          },
          "search":   { "enable": true },
          "metrics":  { "enable": true, "prometheus": { "path": "/metrics" } },
          "scrub":    { "enable": true, "interval": "24h" }
        },
        "log": {
          "level":  "info",
          "output": "/dev/stdout"
        }
      }
    EOT
  }

  depends_on = ["etcd", "dex"]

  health {
    type   = "http"
    cmd    = "https://$VM_IP:8080/v2/_health"
    period = "5s"
  }
}
