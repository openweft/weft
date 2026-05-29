# cloud-init

Pure-Go builder for [cloud-init](https://cloudinit.readthedocs.io/) NoCloud seed images.

Generates minimal ISO 9660 images containing `meta-data` and `user-data`, directly in memory — no external tools required.

## Module

```
github.com/openweft/cloud-init
```

## API

```go
// BuildCloudInitISO returns a ready-to-write ISO image (NoCloud datasource).
func BuildCloudInitISO(instanceID, hostname, userData string) ([]byte, error)

// BuildSSHCloudConfig returns user-data that authorizes the given SSH public
// keys and optionally runs extra commands on first boot.
func BuildSSHCloudConfig(sshPubKeys []string, extraCommands string) string
```

## Usage

```go
import cloudinit "github.com/openweft/cloud-init"

userData := cloudinit.BuildSSHCloudConfig([]string{pubKey}, "")
iso, err := cloudinit.BuildCloudInitISO("instance-001", "my-vm", userData)
if err != nil {
    log.Fatal(err)
}
os.WriteFile("seed.iso", iso, 0o644)
```

## Used by

- [`weft`](../weft) — injects cloud-init ISO into provisioned VMs
