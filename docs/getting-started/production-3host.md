# Day-0: deploy a weft 3-DC cluster in production

Operator-focused walkthrough: zero to 3 converged Debian hosts, fronted
by Caddy, OIDC in place, observability wired up, first VM provisioned.

Fixed prerequisites:
- 3 bare-metal or IaaS machines (Debian 12+, KVM enabled in BIOS, 1
  routable IP per host, 1 dedicated disk for `/var/lib/weft/`).
- 1 workstation (Linux or macOS) with SSH access to the 3 hosts,
  where the operator-side `weft` binary lives.
- 1 OIDC IdP reachable from the 3 hosts (Dex, Keycloak, Okta, Auth0 —
  see `../operations/sso/`).
- 1 domain name under your control (DNS A record per host or wildcard
  to a VIP — your call).

Estimated time budget: 2 h if the IdP is already configured, 4 h if
you provision Keycloak alongside.

## Step 1 — install the CLI on the workstation

```sh
gh release download v0.1.0 --repo openweft/weft \
  --pattern "weft-$(uname -s | tr A-Z a-z)-$(uname -m | sed s/x86_64/amd64/)_*.tar.gz"
tar xzf weft-*.tar.gz
sudo mv weft /usr/local/bin/
weft version
```

Verify the signature before executing — see
[../operations/cosign-verify.md](../operations/cosign-verify.md) for
the `cosign verify-blob` command to run beforehand.

## Step 2 — provision the 3 hosts

Drop `examples/cloud-init/debian-host.yaml` into your install seed
(custom ISO, PXE, or drop into Tart/Proxmox). The file installs the
agent-side `weft` binary, creates the systemd service, opens the
firewall ports (etcd 2379/2380, WireGuard mesh UDP, gRPC 9090, metrics
9101) and leaves the agent waiting for configuration.

When cloud-init finishes, each host is reachable via SSH (using the key
you supplied through `ssh_authorized_keys`) but the agent has no config
yet — it logs `awaiting /etc/weft/weft.hcl`.

Verify from the workstation:

```sh
for ip in 10.0.0.11 10.0.0.12 10.0.0.13; do
  ssh admin@$ip systemctl is-active weft.service
done
# expected: 3× "activating" (waiting for config)
```

## Step 3 — write `cluster.hcl`

On the workstation, in a dedicated directory:

```hcl
cluster "prod" {
  overlay { subnet = "10.9.0.0/24" }

  agent_config {
    socket = "/var/run/weft/weft.sock"

    oidc {
      issuer    = "https://sso.example.com/realms/weft"
      client_id = "weft-agent"
    }

    storage {
      backend = "etcd"
      etcd {
        endpoints = [
          "http://10.9.0.11:2379",
          "http://10.9.0.12:2379",
          "http://10.9.0.13:2379",
        ]
      }
    }

    proxy {
      enabled = true
      acme    { email = "ops@example.com" }
    }

    metrics_listen = ":9101"
    audit_log      = "/var/log/weft/audit.jsonl"
  }

  host "h1" { address = "10.0.0.11" dc = "dc1" hypervisor = "qemu" ssh { user = "admin" } }
  host "h2" { address = "10.0.0.12" dc = "dc2" hypervisor = "qemu" ssh { user = "admin" } }
  host "h3" { address = "10.0.0.13" dc = "dc3" hypervisor = "qemu" ssh { user = "admin" } }
}
```

Substitute your own IPs / domains / IdP. The `proxy` block activates
Caddy as a supervisor with ACME — your 3 hosts must be reachable on
TCP 80/443 for HTTP-01 validation, otherwise use a DNS challenge
(see `../operations/proxy.md`).

For the `hypervisor` field: `qemu` is the portable Linux/KVM default.
`vz` only makes sense on a macOS host (Apple Virtualization) — not
recommended in production, see the `env_no_nested_virt` memory.

## Step 4 — `weft up`

```sh
weft up -f cluster.hcl --apply
```

The planner runs: provision SSH key to each host → push
`/etc/weft/weft.hcl` → pull the OCI images (`weft-microvm-kernel`,
drivers, `weft-proxy`) on each host → start `weft.service` → form the
etcd quorum → enable Caddy → register each host in the registry.

Expected output (~3-5 min):

```
[1/3] h1: weft.hcl pushed, agent started, joined cluster
[2/3] h2: weft.hcl pushed, agent started, joined cluster
[3/3] h3: weft.hcl pushed, agent started, joined cluster
cluster prod ready (3 hosts, quorum: 3/3, proxy: enabled)
```

If a host refuses to join, see
[../operations/ha-failover.md](../operations/ha-failover.md#partition).

## Step 5 — verify convergence

```sh
weft host ls
# expected: 3 hosts state=Running, az=dc{1,2,3}

weft cluster status
# expected: etcd quorum=3/3, proxy=running, drivers=qemu

curl -s https://prod.example.com:9101/metrics | head -10
curl -s https://prod.example.com:9101/metrics | head -10
curl -s https://prod.example.com:9101/metrics | head -10
# expected: prometheus exposition format, grpc_server_* family present
```

If Prometheus is already deployed, scrape the 3 endpoints with the
`instance=<dc>` label. Dashboard import:
[../operations/grafana/README.md](../operations/grafana/README.md).

## Step 6 — provision the first VM

```sh
weft instance start \
  --project default \
  --name canary \
  --image ghcr.io/openweft/debian-12-cloud:latest \
  --cpu 2 --memory 2048 \
  --network default
```

Once Running:

```sh
weft instance ls
weft instance status canary
weft instance logs canary --follow
```

The scheduler placed the VM on one of the 3 hosts according to
constraints (no default SchedulingRule → simple balancing by available
CPU).

## Step 7 — deploy the webui

The webui (HuMA + Svelte) is a separate binary. Three deployment
options:

1. **Bare metal**, alongside each agent (systemd unit shipped in
   `examples/cloud-init/`).
2. **Kubernetes**, via the Helm chart at
   [../../charts/weft-agent/](../../charts/weft-agent/) which also
   starts the webui as a sidecar.
3. **Standalone container**:
   `docker run -p 8088:8088 ghcr.io/openweft/weft-webui:v0.1.0 \
     -e WEBUI_OIDC_ISSUER=... -e WEBUI_OIDC_CLIENT_ID=... \
     -e WEBUI_AGENT_ADDR=10.9.0.11:9090`

Configure a Caddy route for `https://weft.example.com` → webui
(edited through the registry route; see `../operations/proxy.md`).

## Step 8 — wire up Terraform for workloads

On the workstation or developer machine:

```hcl
terraform {
  required_providers {
    weft = { source = "openweft/weft" version = "~> 0.1" }
  }
}

provider "weft" {
  agent_addr = "10.9.0.11:9090"
  # service-account OIDC token, see docs/operations/rbac.md
}

resource "weft_volume" "data" {
  project  = "default"
  name     = "app-data"
  size_gib = 50
}
```

`terraform init && terraform apply` provisions resources as plain
IaC. 33 of 98 RPCs are currently exposed through the provider — the
rest live under the CLI or the webui (see
`../../GAPS.md`).

## Step 9 — install a first catalogue plugin (optional)

If you need CI runners right away:

```sh
weft plugin list
weft plugin install gitlab-runners-ha \
  --input registration_token=$(cat /tmp/gitlab-token) \
  --input gitlab_url=https://gitlab.example.com
```

The plugin starts 3 runner VMs spread across the 3 DCs with strong
anti-affinity. See [../catalogue/README.md](../catalogue/README.md)
for the other plugins (github-runners-ha, forgejo-runners-ha,
jupyterhub-ha).

## Final day-0 checklist

Tick `OK` when each line is green:

- [ ] 3 hosts `weft host ls` state=Running
- [ ] etcd quorum = 3/3 (`etcdctl endpoint health --cluster`)
- [ ] Caddy returns 200 on `https://<your-domain>/`
- [ ] OIDC web login works (`https://weft.example.com/` → IdP → callback)
- [ ] `/metrics` responds on each host
- [ ] Prometheus scrapes the 3 endpoints
- [ ] Grafana dashboard imported and populated
- [ ] 1 canary VM starts, ping survives
- [ ] Reflink snapshot works (`weft volume snapshot create --volume=<uuid> --name=test`)
- [ ] First etcd backup taken (`docs/operations/etcd-backup.md` step 1)
- [ ] Audit log written to `/var/log/weft/audit.jsonl` on every login

## Day-1 and beyond

The follow-up loops are documented in separate runbooks:

- etcd backup / restore — [../operations/etcd-backup.md](../operations/etcd-backup.md)
- Off-host snapshot backup — [../operations/backup.md](../operations/backup.md)
- HA failover — [../operations/ha-failover.md](../operations/ha-failover.md)
- Disaster recovery (lost quorum) — [../operations/disaster-recovery.md](../operations/disaster-recovery.md)
- Rolling upgrade v0.X → v0.Y — [../operations/upgrade.md](../operations/upgrade.md)
- GPU scheduling H200 / RTX 6000 Ada — [../operations/gpu-scheduling.md](../operations/gpu-scheduling.md)
- Tenant quotas — [../operations/tenant-quotas.md](../operations/tenant-quotas.md)
- RBAC + audit log — [../operations/rbac.md](../operations/rbac.md)
- Cosign verification — [../operations/cosign-verify.md](../operations/cosign-verify.md)
- Observability — [../operations/observability.md](../operations/observability.md)

## Next steps — growing the cluster

Once the 3-DC is in place, the cluster keeps evolving:

- **Scale-out** — adding a 4th (or Nth) host: see
  [../operations/scale-out.md](../operations/scale-out.md). Covers
  both paths (convergent `weft up --apply` vs. explicit
  `weft host register`) and growing etcd 3 → 5 → 7.
- **Drain + remove** — taking a host out of the cluster (hardware
  failure, replacement, decommission): see
  [../operations/drain-remove-host.md](../operations/drain-remove-host.md).
  Cordon → drain → `weft host rm` → HCL clean-up → `etcdctl member remove`.

## What is not (yet) covered

- Bare-metal testing outside Tart: the 3-host harness
  (`tests/integration/3host/`) compiles but has never been run
  against real metal — metal-specific bug manifestations are
  operator-side discoveries.
- Fine-grained per-VM device passthrough (PCI, USB): the groundwork
  is in place on the QEMU driver side, the API surface is still to come.
- Multi-cluster federation: a single cluster (1-host or 3-DC) in V1.

If you get stuck: open an issue on
[github.com/openweft/weft](https://github.com/openweft/weft) with the
output of `weft cluster status -o json` plus `journalctl -u weft.service`
from the affected hosts.
