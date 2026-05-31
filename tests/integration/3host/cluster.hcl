// cluster.hcl — fixture consumed by the 3-host integration harness.
//
// Targets the three local Tart Debian VMs at .11/.12/.13 (the env documented
// in env_no_nested_virt: dev runs inside macOS Tart, Apple-VZ cannot nest,
// QEMU/TCG can — so this fixture pins hypervisor = "qemu" on every host).
//
// The IP prefix is overrideable via $WEFT_INTEGRATION_HOSTS_PREFIX (default
// "192.168.64") so the same fixture works against another Tart subnet. The
// last octets are fixed at 11/12/13 (the agent's harness fills them in via
// envsubst when ${WEFT_INTEGRATION_HOSTS_PREFIX} is set, otherwise the file
// is consumed as-is by `weft up`).
//
// The agent_config { } block pushes an embedded-etcd configuration to each
// host : 3 members, one per DC, peer URLs over the underlay. Re-running
// `weft up` against an already-converged cluster is a no-op (planner
// computes desired − observed), so the harness can call up twice safely.

cluster "weft-integration-3host" {
  overlay { subnet = "10.9.0.0/24" }

  // Cluster-wide agent_config — pushed to /etc/weft/weft.hcl on every host
  // before the agent starts (PushAgentConfig action, see cluster/plan.go).
  // The embedded-etcd 3-member quorum is what we assert in TestEtcdQuorum.
  agent_config {
    socket = "/var/run/weft/weft.sock"
    storage {
      backend = "etcd"
      etcd {
        // Endpoints are the three peers' client-URLs on the overlay. The
        // harness rewrites these per-host if a non-default overlay was
        // requested ; the default is the matching 10.9.0.x address.
        endpoints = [
          "http://10.9.0.11:2379",
          "http://10.9.0.12:2379",
          "http://10.9.0.13:2379",
        ]
      }
    }
  }

  host "h1" {
    address    = "192.168.64.11"
    dc         = "dc1"
    hypervisor = "qemu"
    ssh {
      user = "admin"
    }
  }

  host "h2" {
    address    = "192.168.64.12"
    dc         = "dc2"
    hypervisor = "qemu"
    ssh {
      user = "admin"
    }
  }

  host "h3" {
    address    = "192.168.64.13"
    dc         = "dc3"
    hypervisor = "qemu"
    ssh {
      user = "admin"
    }
  }
}
