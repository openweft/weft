# Cloud-init reference template for a weft host

This directory holds a cloud-init `#cloud-config` that brings a fresh
Debian-arm64 (or Ubuntu) box to a weft-ready state in one shot: weft user,
agent binary on PATH, systemd unit, shared microVM kernel pre-pulled,
`weft-proxy` staged.

When the cloud-init run finishes, the host is something `weft up --apply`
can pilot — the operator just edits `cluster.hcl` to point at it and runs
the bring-up command.

## When to use this

- Greenfield Debian 12 (arm64 or amd64) or Ubuntu 22.04+ host.
- Cloud providers that accept user-data: AWS EC2, Hetzner Cloud,
  Equinix Metal, Scaleway, OpenStack, Vultr, ...
- Bare metal via `cloud-localds` + a seed ISO.

## Files

| File | Purpose |
|---|---|
| `debian-host.yaml` | The cloud-init config — hand this to the cloud provider |
| `systemd/weft-agent.service` | Standalone systemd unit, for operators provisioning hosts outside the cloud-init flow (Ansible, Packer, hand-built images) |

## What it sets up

1. `weft` system user (uid 1000), sudoer, in the `kvm` group.
2. Packages: `curl`, `ca-certificates`, `wireguard-tools`, `iproute2`,
   `qemu-system-{arm,x86}`, `qemu-utils`, `jq`. Plus `oras` installed
   directly from GitHub releases (no Debian package).
3. `/etc/weft/weft.hcl` skeleton (storage backend, proxy block).
4. `weft-proxy` binary pulled from
   `ghcr.io/openweft/weft-proxy:v0.1.0` (OCI artifact, oras pull)
   and installed to `/usr/local/bin/weft-proxy`.
5. Shared microVM kernel pulled via
   `weft microvm pull-kernel ghcr.io/openweft/weft-microvm-kernel:v0.1.0`
   into `$XDG_DATA_HOME/weft-microvm/kernel`.
6. `weft-agent.service` enabled and started, running `weft agent` as the
   `weft` user with `CAP_NET_ADMIN` ambient capability (the kernel
   WireGuard backend needs it to program the overlay).

## Operator placeholders

Search `debian-host.yaml` for `TODO` — there are four:

| TODO | Where | What to fill |
|---|---|---|
| operator SSH keys | `users[0].ssh_authorized_keys` | The public keys `weft up`'s SSH client will dial in with. Must match the private key referenced by `cluster.hcl`'s `ssh { key = "..." }` block. |
| `cluster_name` | `/etc/weft/weft.hcl` | Identifies the cluster; matches `cluster.hcl`'s `name`. |
| etcd endpoints | `/etc/weft/weft.hcl` `storage.etcd.endpoints` | Your 3-DC control plane's etcd URLs. For a single-host dev cluster, switch to `backend = "file"` and drop the block. The `proxy.storage.endpoints` field usually mirrors this. |
| `weft` binary | `runcmd` step 2 | The openweft/weft repo does not yet publish release artefacts. Until that lands, scp `/usr/local/bin/weft` onto the host manually before the agent unit will stay up. |

You will probably also want to set `--az` and `--rack` flags on `weft
agent` (via cluster.hcl `host { dc, rack }` — see `cluster/ssh.go`'s
`renderAction`). For a single-host or single-rack deployment the defaults
are fine.

## Registry access

The kernel / driver / proxy OCI artifacts under
`ghcr.io/openweft/*` are public — no auth is needed for the canonical
openweft images. If you mirror these into a private registry, log in
ahead of the cloud-init run:

```bash
# Option A: docker config (oras reads this automatically)
docker login ghcr.io -u <user> -p <PAT>

# Option B: oras-native
oras login ghcr.io -u <user> -p <PAT>
```

`oras` looks at `~/.docker/config.json` and `~/.config/containers/auth.json`.
In a cloud-init context the simplest path is to inject the config file
via `write_files` ahead of the `runcmd` block that calls `oras pull`.

## Debugging a failed bring-up

The agent's stderr/stdout go to the journal. Useful invocations:

```bash
# Stream the agent log live (what you want 90% of the time).
journalctl -u weft-agent -f

# Last 200 lines plus current status.
systemctl status weft-agent
journalctl -u weft-agent -n 200 --no-pager

# Cloud-init's own log (runcmd failures land here).
sudo cat /var/log/cloud-init-output.log
sudo cat /var/log/cloud-init.log

# Verify the binaries landed.
ls -l /usr/local/bin/{weft,weft-proxy,oras}
weft --version
weft-proxy version

# Verify the kernel pull succeeded.
sudo -u weft ls -l /home/weft/.local/share/weft-microvm/

# WireGuard sanity (after the first `weft up --apply`).
sudo wg show
ip -4 addr show
```

Common failures:

- **`weft-agent.service` fail-looping with "no such file": **
  `/usr/local/bin/weft` is missing (release-artefact TODO above). scp it on.
- **`oras pull` 401 / 403:** GHCR thinks the artifact is private, or
  you're mirroring to an auth-gated registry. See "Registry access".
- **`pull-kernel` succeeds but the agent can't find it:** XDG path
  mismatch — the agent and the puller need the same `HOME`/`XDG_DATA_HOME`.
  Always run the puller as the `weft` user (`sudo -u weft -H ...`).
- **WireGuard programs but no peers ever connect:** `CAP_NET_ADMIN`
  missing from the unit, or `wireguard-tools` not installed.

## Where this fits in the bring-up

1. Provision N hosts (1 or 3) with this cloud-init.
2. Edit each host's `/etc/weft/weft.hcl` placeholders (or bake them
   into the cloud-init via Jinja templating before submitting).
3. On the operator box, author `cluster.hcl` listing the hosts.
4. `weft up -f cluster.hcl` (dry-run) then `weft up --apply -f cluster.hcl`.

See `docs/operations/cloud-init.md` for the doc-side pointer and
`cluster/README.md` for the bring-up semantics.
