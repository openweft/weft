# weft infra plan — etcd control-plane cluster
#
# Three micro-VMs, one per DC, forming a single etcd cluster.
# Each VM mounts a dedicated persistent volume for /var/lib/etcd
# so an etcd-VM restart preserves the data; deleting the VM
# without deleting the volume is recoverable.
#
# Schema is loosely modeled on Nomad / k8s manifests, expressed
# in HCL per [[hcl-over-json]].

service "etcd" {
  description = "3-DC etcd cluster — the cloud platform control plane"
  oci_image   = "ghcr.io/openweft/weft-etcd:v3.6.0"

  # Resources per VM (3 total).
  resources {
    cpu_count  = 1
    memory_mib = 1024
  }

  # Persistent state: one volume per replica, each carved out of
  # the local storage pool by the DC. Volume UUIDs are stable so
  # an etcd-VM can be re-deployed on the same volume after a host
  # reboot or after a failed kernel patch.
  volume {
    mount    = "/var/lib/etcd"
    uuid     = "etcd-data-dc1"
    size_gib = 32
  }
  volume {
    mount    = "/var/lib/etcd"
    uuid     = "etcd-data-dc2"
    size_gib = 32
  }
  volume {
    mount    = "/var/lib/etcd"
    uuid     = "etcd-data-dc3"
    size_gib = 32
  }

  # Private control-plane subnet — only weft / dex / zot speak to
  # etcd. User workloads never see this network.
  network {
    name      = "control-plane"
    static_ip = ["10.255.1.10", "10.255.1.11", "10.255.1.12"]
  }

  # Kernel cmdline override merged into weft-microvm-init's default.
  # The OCI image's entrypoint is etcd itself; the config below
  # is materialised at deploy time and exposed via virtio-fs.
  cmdline = "weft.rootfs=virtiofs:rootfs0 weft.config=virtiofs:cfg"

  # Service-specific config rendered by weft at deploy time. The
  # tokens marked $REPLICA, $PEERS, $PRIVATE_IP, $DC are filled
  # in per VM. See README.md for the bootstrap detail.
  config_file {
    path     = "/etc/etcd/etcd.conf.yaml"
    template = <<-EOT
      name: 'etcd-$DC'
      data-dir: /var/lib/etcd
      listen-peer-urls: 'https://$PRIVATE_IP:2380'
      listen-client-urls: 'https://$PRIVATE_IP:2379'
      initial-advertise-peer-urls: 'https://$PRIVATE_IP:2380'
      advertise-client-urls: 'https://$PRIVATE_IP:2379'
      initial-cluster: '$PEERS'
      initial-cluster-state: 'new'
      initial-cluster-token: 'weft-control-plane'
      client-transport-security:
        cert-file:        /etc/etcd/tls/server.crt
        key-file:         /etc/etcd/tls/server.key
        trusted-ca-file:  /etc/etcd/tls/ca.crt
        client-cert-auth: true
      peer-transport-security:
        cert-file:        /etc/etcd/tls/peer.crt
        key-file:         /etc/etcd/tls/peer.key
        trusted-ca-file:  /etc/etcd/tls/ca.crt
        peer-client-cert-auth: true
      auth-token: 'jwt,pub-key=/etc/etcd/dex.pub,priv-key=,sign-method=RS256'
    EOT
  }

  # No upstream dependencies — etcd is the foundation. The bootstrap
  # walks this DAG and deploys depends-on-nothing services first.
  depends_on = []

  # Sanity probes weft polls before declaring the VM Ready.
  health {
    type   = "exec"
    cmd    = "/usr/local/bin/etcdctl endpoint health --insecure-skip-tls-verify"
    period = "5s"
  }
}
