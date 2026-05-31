# weft-agent Helm chart

Minimal Helm chart for deploying `weft-agent` (openweft's hypervisor
control daemon) into a Kubernetes cluster, using the multi-arch OCI
image `ghcr.io/openweft/weft:<tag>` that the release workflow publishes
and cosign-signs on every `v*` tag.

## Deployment models

The chart supports two models. Pick one based on what the cluster is
meant to do.

### 1. Host-network mode (default, `hostNetwork: true`)

This is the model the chart is **biased toward** and the one the
existing `examples/cloud-init/debian-host.yaml` mirrors. The agent
pod shares the host's network namespace and holds `CAP_NET_ADMIN`
so it can:

- program WireGuard tunnels for the overlay mesh,
- manage iproute2 / netlink state for VM bridges,
- supervise the local hypervisor (Apple VZ or QEMU/KVM) on the host
  kernel directly.

Operators typically label hypervisor-capable nodes (for example
`openweft.io/hyperviser=true`) and bind a `nodeSelector` + matching
`tolerations` to land one agent per host. A `replicaCount` of 1 with a
nodeSelector + anti-affinity is functionally equivalent to a DaemonSet
and easier to upgrade.

```yaml
nodeSelector:
  openweft.io/hyperviser: "true"
tolerations:
  - key: openweft.io/hyperviser
    operator: Exists
    effect: NoSchedule
hostNetwork: true
```

### 2. Control-plane-only mode (`hostNetwork: false`)

Use this when Kubernetes itself is **not** the hypervisor substrate —
i.e. the actual VMs run on dedicated hosts managed by their own
`weft-agent` (systemd or otherwise), and you only want a pod here to
expose the gRPC API + `/metrics` to in-cluster consumers (webui, CI,
Terraform provider).

In this mode the agent does no WireGuard / bridge programming locally;
it acts as a thin façade over the etcd-backed control plane. Drop
`CAP_NET_ADMIN` from `values.yaml` if your security posture requires it
(the chart keeps it on by default for symmetry with the host-network
path).

```yaml
hostNetwork: false
dnsPolicy: ClusterFirst
proxy:
  enabled: false
```

## Values worth reviewing

- `image.tag` — defaults to `.Chart.appVersion`. Pin to a specific
  release for reproducibility; cosign-verify before pulling in
  production (see release workflow).
- `oidc.issuer` — leave empty in dev (no auth); set to your dex / IdP
  for production. Mirrors `WEFT_OIDC_ISSUER`.
- `etcd.endpoints` — list of HA control-plane endpoints. Empty list
  falls back to `backend = "file"` (single-host dev).
- `proxy.enabled` — turns on the embedded Caddy reverse proxy. Usually
  disabled in-cluster because Kubernetes Ingress already terminates
  TLS upstream.
- `metrics.listen` / `metrics.serviceMonitor.enabled` — Prometheus
  scrape config. The ServiceMonitor template is gated on both this
  flag and the presence of the `monitoring.coreos.com` CRDs in the
  cluster.
- `persistence.size` — PVC for `/var/lib/weft/` (embedded etcd data
  dir, proxy state, imagestore cache). Default 10Gi.

## Install

```sh
helm install weft-agent ./charts/weft-agent \
  --namespace weft-system --create-namespace \
  --set image.tag=v0.1.0 \
  --set oidc.issuer=https://dex.internal.example.com \
  --set etcd.endpoints={http://etcd-0.etcd:2379,http://etcd-1.etcd:2379,http://etcd-2.etcd:2379}
```

## Verify the image

The release workflow signs each per-arch image and the multi-arch
index with cosign keyless. Before deploying:

```sh
cosign verify ghcr.io/openweft/weft:v0.1.0 \
  --certificate-identity-regexp 'https://github.com/openweft/weft/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Source of truth

The HCL rendered into the ConfigMap mirrors the schema in
`cmd/weft/config.go` and the cloud-init template at
`examples/cloud-init/debian-host.yaml`. When that schema grows new
blocks (additional storage backends, mesh tuning, etc.), update the
ConfigMap template in lockstep.
