# Cloud-init host bring-up

`examples/cloud-init/` ships a reference cloud-init `#cloud-config`
that turns a fresh Debian-arm64 (or Ubuntu) instance into a host that
`weft up --apply` can drive — without the operator having to chase
binaries, ambient capabilities, or systemd-unit boilerplate.

It is one tool in the host-provisioning bag, not the only one. Use it
when you don't already have a Packer / Ansible / image-baking pipeline
and want to go from "blank cloud VM" to "node weft owns" in one boot.

## What it does

In broad strokes:

- creates the unprivileged `weft` service account (uid 1000),
- installs the runtime deps (oras, wireguard-tools, qemu, jq, ...),
- drops a skeleton `/etc/weft/weft.hcl`,
- pulls `weft-proxy` from `ghcr.io/openweft/weft-proxy` via oras,
- pre-pulls the shared microVM kernel via
  `weft microvm pull-kernel ghcr.io/openweft/weft-microvm-kernel`,
- writes and enables `weft-agent.service` with `CAP_NET_ADMIN` ambient
  cap (needed by the kernel WireGuard backend),
- leaves the operator a handful of `TODO`s to fill (cluster name,
  etcd endpoints, SSH keys, weft binary URL).

The full file is `examples/cloud-init/debian-host.yaml`. The systemd
unit is also shipped standalone at
`examples/cloud-init/systemd/weft-agent.service` for adoption outside
the cloud-init flow.

## Operator usage

See [`examples/cloud-init/README.md`](../../examples/cloud-init/README.md)
for the step-by-step: the placeholders to fill, registry-auth notes,
and the `journalctl -u weft-agent` debug recipes.

## Where it sits in the bring-up

The cloud-init handles host-local prep — everything that happens before
`weft up` reaches the box. The actual cluster convergence (etcd quorum,
overlay mesh, replica placement) is `weft up --apply`'s job and is
documented in `cluster/README.md`. The two are designed to compose:
cloud-init lands the prerequisites `weft up`'s SSH command stream
assumes (per `cluster/ssh.go`'s `renderAction`).
