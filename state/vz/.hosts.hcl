# weft host registry — cluster-wide compute node inventory.
# Each weft-agent registers itself here on startup. UUID is
# immutable; hostname unique cluster-wide; state managed by
# the control plane (heartbeat → active/down).

 host "e053b4d0-c9ee-4ad8-bd3b-2204790fb47b" {
  hostname         = "mbp-10841428"
  hypervisor       = "apple-vz"
  architecture     = "arm64"
  network_types    = ["nat", "bridged", "isolated", "mesh"]
  volume_backends  = ["file"]
  state            = "active"
  last_seen_at     = "2026-08-17T22:49:48.106491Z"
  created_at       = "2026-08-17T12:55:03.657308Z"
  wg_public_key    = "CGPZKZEUtGFk7FrcXe8J8SFLVvES+2J02AeEHVoy6iM="
  wg_overlay_index = 7
  agent_version    = "dev"
  driver_versions = {
    apple-vz = "dev"
  }
  cpu_count = 16
}

