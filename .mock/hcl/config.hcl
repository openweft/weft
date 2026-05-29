// Local-dev example config for `weft agent --config-dir .mock/hcl`.
//
// This is the all-in-one single-host default: file storage + in-process
// event bus, no etcd / NATS required. Edit the ssh keypair path and the
// image/VM definitions to taste. The config dir is optional — the daemon
// serves the gRPC API with or without it; it only pre-declares the images
// and VMs that `weft image …` / `weft instance …` operate on.
version = "1"

mock "local" {
  // Default SSH identity injected into every VM that doesn't override it.
  // The file only has to exist when a VM is actually provisioned, not at
  // config-load time — so this validates even on a fresh machine.
  ssh {
    user    = "ubuntu"
    keypair = "~/.ssh/id_ed25519"
  }

  // Pull up to 4 images concurrently.
  parallelism = 4
}

// Named SSH keypair VMs can reference via `keypair = keypair.dev`.
keypair dev {
  file_path = "~/.ssh/id_ed25519"
}

// An image to pull/cache. `weft image pull` materialises it locally.
image debian {
  from = "docker.io/library/debian:13"
}

// A single VM definition. `weft instance list` shows it; `weft instance
// start debian-vm` provisions + boots it from the cached image above.
vms debian-vm {
  count = 1
  cpu   = 2
  mem   = 2048

  disk {
    from = "image.debian.from"
    size = "20Gi"
  }

  ssh {
    user    = "debian"
    keypair = keypair.dev
  }
}
