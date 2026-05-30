# weft host registry — cluster-wide compute node inventory.
# Each weft-agent registers itself here on startup. UUID is
# immutable; hostname unique cluster-wide; state managed by
# the control plane (heartbeat → active/down).

 host "ec13e2be-dd41-40ec-82aa-3f8d148e368e" {
  hostname        = "Manageds-Virtual-Machine.local"
  hypervisor      = "apple-vz"
  architecture    = "arm64"
  network_types   = ["nat", "bridged", "isolated", "mesh"]
  volume_backends = ["file"]
  state           = "active"
  last_seen_at    = "2026-05-27T19:05:53.540652Z"
  created_at      = "2026-05-26T20:42:04.49814Z"
}

