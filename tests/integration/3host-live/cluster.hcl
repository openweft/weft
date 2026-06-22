// cluster.hcl — reconstructed live 3-DC cluster description for
// dc1-r1-h1 / dc2-r1-h1 / dc3-r1-h1 (Tart Debian arm64 VMs on
// 192.168.105.0/24 per dc_tart_subnet memory). External etcd 3.5.16
// installed manually via apt on every host ; agent_config points at
// the 3-member quorum on the underlay.
//
// hypervisor = "qemu" everywhere because Apple-VZ can't nest under
// the Tart host (env_no_nested_virt). socket = user-writable path
// because the agent runs as user-mode (no /var/run/weft permission).

cluster "openweft-live-3dc" {
  microvm {
    pod_initrd_ref = "ghcr.io/openweft/weft-microvm-pod-initrd:v0.2.1"
  }
  overlay { subnet = "10.9.0.0/24" }

  # Pin the infra services (etcd / nats / coredns / dex / zot / webui …) to
  # the two hosts explicitly labelled `role=control-plane` below. h3 stays
  # general-purpose : it joins the mesh, runs user microVMs, but no infra
  # replica lands on it. See docs/operations/scale-out.md (« Pinning the
  # control plane »).
  control_plane {
    require_properties = ["role=control-plane"]
  }

  agent_config {
    socket = "/home/admin/.weft/weft.sock"
    storage {
      backend = "etcd"
      etcd {
        endpoints = [
          "http://192.168.105.11:2379",
          "http://192.168.105.12:2379",
          "http://192.168.105.13:2379",
        ]
      }
    }
  }

  host "h1" {
    address    = "192.168.105.11"
    dc         = "dc1"
    hypervisor = "qemu"
    properties = { role = "control-plane", storage = "nvme" }
    ssh {
      user = "admin"
      key  = "/Users/admin/.ssh/id_ed25519"
    }
  }

  host "h2" {
    address    = "192.168.105.12"
    dc         = "dc2"
    hypervisor = "qemu"
    properties = { role = "control-plane", storage = "nvme" }
    ssh {
      user = "admin"
      key  = "/Users/admin/.ssh/id_ed25519"
    }
  }

  host "h3" {
    address    = "192.168.105.13"
    dc         = "dc3"
    hypervisor = "qemu"
    properties = { role = "control-plane", storage = "nvme" }
    ssh {
      user = "admin"
      key  = "/Users/admin/.ssh/id_ed25519"
    }
  }
}
