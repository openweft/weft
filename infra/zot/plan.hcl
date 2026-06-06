# weft infra plan — zot OCI registry
#
# 1 VM per DC, sync-mirroring between them. Each VM uses S3-
# compatible object storage by default (matches the
# cloud_platform_direction.md "default to cloud-native"
# preference); a local-fs override is provided for single-host
# dev / air-gapped lab setups.
#
# Acts as the EGRESS CACHE/PROXY for every external OCI registry
# the cluster pulls from (docker.io, ghcr.io, quay.io,
# registry.k8s.io, public.ecr.aws). The `extensions.sync` block
# below registers each upstream with `onDemand = true` : the
# first pull for an image fetches from upstream and caches
# inside zot ; every subsequent pull (any host, any DC, any
# replica) serves from local storage and never crosses the
# cluster boundary. This is the boundary every weft microvm
# pull should hit first — see weft/imagestore + the
# weft-microvm pull-path that routes through the local zot
# IP triplet 10.255.2.30/31/32 before touching upstream.

service "zot" {
  description = "OCI Distribution registry — kernel/initrd/plans + signed artefacts"
  oci_image   = "ghcr.io/openweft/weft-zot:v2.1.0"

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
            "credentialsFile": "/etc/zot/sync-credentials.json",
            "registries": [
              {
                "urls":      ["https://upstream-zot.$PEER_DC.$BASE_DOMAIN"],
                "onDemand":  true,
                "tlsVerify": true,
                "content":   [{ "prefix": "**" }]
              },
              {
                "urls":      ["https://registry-1.docker.io"],
                "onDemand":  true,
                "tlsVerify": true,
                "content":   [
                  { "prefix": "library/**" },
                  { "prefix": "**", "destination": "/dockerhub" }
                ]
              },
              {
                "urls":      ["https://ghcr.io"],
                "onDemand":  true,
                "tlsVerify": true,
                "content":   [{ "prefix": "**" }]
              },
              {
                "urls":      ["https://quay.io"],
                "onDemand":  true,
                "tlsVerify": true,
                "content":   [{ "prefix": "**" }]
              },
              {
                "urls":      ["https://registry.k8s.io"],
                "onDemand":  true,
                "tlsVerify": true,
                "content":   [{ "prefix": "**" }]
              },
              {
                "urls":      ["https://public.ecr.aws"],
                "onDemand":  true,
                "tlsVerify": true,
                "content":   [{ "prefix": "**" }]
              },
              {
                "urls":      ["https://codeberg.org"],
                "onDemand":  true,
                "tlsVerify": true,
                "content":   [{ "prefix": "forgejo/**" }]
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
