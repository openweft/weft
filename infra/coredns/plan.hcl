# weft infra plan — CoreDNS HA
#
# Three micro-VMs, one per AZ, joined behind a stable anycast-
# style triplet of static IPs that every other infra service
# (and every tenant VM) uses for name resolution. The cluster
# is masterless : each CoreDNS instance answers queries
# independently, reading zone data from the same etcd cluster
# that backs the rest of the control plane.
#
# Bootstrap order ([[infra-in-micro-vms]]) :
#   file-weft → etcd → **coredns** → dex → zot → nats → self-promote
#
# CoreDNS lands right after etcd : it's a hard dependency for
# every name-based reference in the configs above (dex's
# upstream LDAP, zot's bearer realm pointing at dex,
# nats-cluster peers, …). Anything that came up before CoreDNS
# has to use IPs ; everything after can use names.

service "coredns" {
  description = "CoreDNS — cluster-internal name resolution (weft.internal zone, etcd-backed)"
  oci_image   = "coredns/coredns:1.11.3"

  # CoreDNS is light : tiny Go binary, ~30 MB RSS, IO-bound
  # rather than CPU-bound. The 256 MiB / 1 vCPU floor is more
  # than enough headroom for high QPS.
  resources {
    cpu_count  = 1
    memory_mib = 256
  }

  # No persistent volume : zone data lives in etcd ; the
  # rendered Corefile comes from the cfg virtio-fs share. A
  # restart is a zero-state restart.

  # Tenant-services subnet — every service that resolves names
  # (every service post-etcd) sits here, and tenant workloads
  # route to .53 for DNS via their default route.
  network {
    name      = "tenant-services"
    static_ip = ["10.255.3.53", "10.255.3.54", "10.255.3.55"]
  }

  # Standard weft-microvm-init rootfs share + service config mounted at
  # /etc/coredns/Corefile.
  cmdline = "weft.rootfs=virtiofs:rootfs0 weft.config=virtiofs:cfg"

  # The Corefile : two zones, one fallthrough.
  #
  #   weft.internal — authoritative, sourced from etcd at
  #     /weft/dns. Service-discovery writes records there
  #     (e.g. nats-dc1.weft.internal → 10.255.3.30).
  #
  #   .           — recursive resolver. Forwards to operator-
  #     supplied upstreams ($DNS_UPSTREAMS, defaults to the
  #     public anycast resolvers below for bootstrap).
  config_file {
    path     = "/etc/coredns/Corefile"
    template = <<-EOT
      # Bind explicitly to the host-side IP so the health-check
      # endpoint (port 8080) can be reached from the host even
      # before DNS routing is up.
      .:53 {
          errors
          health :8080
          ready  :8181
          prometheus :9153
          loop
          reload
          cache 300
          loadbalance
          forward . 1.1.1.1 9.9.9.9 {
              prefer_udp
              max_fails 3
          }
      }

      weft.internal:53 {
          errors
          cache 30
          reload
          etcd weft.internal {
              endpoints https://etcd-dc1.weft.internal:2379 https://etcd-dc2.weft.internal:2379 https://etcd-dc3.weft.internal:2379
              path /weft/dns
              fallthrough
          }
      }
    EOT
  }

  # CoreDNS reads zones from etcd ; etcd must be up first.
  # Every later service can then resolve names instead of
  # hard-coding IPs.
  depends_on = ["etcd"]

  # Liveness probe : the `health` plugin in the Corefile
  # exposes :8080/health. The deployer substitutes $VM_IP
  # before each poll (see infra/README.md "Health probes").
  health {
    type   = "http"
    cmd    = "http://$VM_IP:8080/health"
    period = "5s"
  }

  # 3-AZ HA cluster, anti-affinity at every level so a single-
  # AZ outage / single-rack outage / single-host failure each
  # take exactly one replica down — the other two keep serving
}
