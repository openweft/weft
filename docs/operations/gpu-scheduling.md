# GPU-aware scheduling

Weft's scheduler matches VMs to hosts across AZ / Rack / Host
axes plus label selectors. This document covers the **GPU
dimension** overlaying those axes for hosts carrying NVIDIA
accelerators.

## Supported fleet

| Model            | Class        | Memory   | MIG | Notes                         |
|------------------|--------------|----------|-----|-------------------------------|
| **H200**         | datacenter   | 141 GiB  | yes | Sliceable into 1g/2g/3g/4g/7g |
| **RTX 6000 Ada** | workstation  | 48 GiB   | no  | Whole-card allocation only    |

Other SKUs work — the model string is operator-defined — but
docs / CLI examples deliberately stay on these two SKUs (no
L40S / H100 / A100).

## The model

Two concerns in `gpu.go` :

1. **Inventory** : `Host.GPUs []GPU` populated by the agent at
   registration via `detectGPUs()` (static stub today — see
   *Real detection*).
2. **Request** : on `ScheduleRequest` :
   - `GPU string` — single-axis SchedulingRule form
     (`"h200"`, `"any-nvidia"`, `"none"`, `""`).
   - `RequestedGPUs []GPURequest` — per-VM vendor + model +
     count + optional MIG slice.

The scheduler is a pure function : (request, inventory) → host.
No claim is made on resources — *exclusive pinning is not
enforced today* (see *Exclusivity boundary*).

## SchedulingRule GPU axis

The GPU axis joins the existing AZ / Rack / Host dimensions :

```hcl
scheduling_rule "training" { az = "us-east-1a" ; gpu = "h200" }
scheduling_rule "inference" { gpu = "any-nvidia" }
scheduling_rule "cpu-only" { gpu = "none" }
```

| `gpu` value      | Meaning                                            |
|------------------|----------------------------------------------------|
| `""`             | No constraint                                      |
| `"none"`         | Host must have NO GPUs                             |
| `"any-nvidia"`   | At least one NVIDIA card                           |
| `"h200"` / `"rtx-6000-ada"` / ... | Case-insensitive SKU match         |

Per [[openweft_nominal_binding]] the GPU axis is one dimension
among many : an explicit VM→rule nominal binding still wins for
counting, regardless of GPU drift.

## Fine-grained per-VM requests

The `requested_gpus` field on a VM expresses a hard need :

```hcl
vm "training-worker" {
  requested_gpus {
    vendor    = "nvidia"
    model     = "H200"
    count     = 4
    mig_slice = "1g.10gb"    # optional ; non-empty requires MIG-capable cards
  }
}
```

Matching rules :

- Every `RequestedGPUs` entry must be satisfied by the host's
  inventory independently.
- `Vendor` is required ; lowercase short tag (`"nvidia"`).
- `Model` is the SKU string OR the wildcard `"any"`.
- `Count` defaults to 1 when zero.
- `MIGSlice` non-empty filters to MIG-capable cards. The H200
  qualifies ; RTX 6000 Ada does not.

A VM whose request cannot be satisfied by any host in the
cluster surfaces a **gRPC `ResourceExhausted`** error — the
same code `tenant_quotas.go` uses for "cluster full". Webui
keys on this code to render a dedicated "no GPU capacity"
toast.

## Exclusivity boundary

**Weft does not yet enforce *exclusive* GPU pinning.** Two VMs
with overlapping GPU requests will both schedule onto one H200
host if its inventory satisfies each request in isolation : the
scheduler holds no claim state.

Deliberate scope cut for the first iteration — the GPU layer
carries inventory + matching, not allocation. Operator owns
the boundary today : informational labels (`gpu_exclusive`),
SchedulingRule count caps, and driver-level MIG partitioning
(nvidia-smi `mig` config). A future commit will add a claim
layer tracking `(host, slot, slice)` per running VM.

> **Update** — the claim layer's *model* has landed : `gpu_alloc.go`
> implements an exclusive `gpuAllocTable` (whole-card claims by PCI
> BDF, MIG claims by mdev UUID) and the exclusivity-aware matcher
> `gpuRequestSatisfiedExcl`. Wiring it into `ScheduleVM` /
> `DeprovisionVM` and persisting it to etcd are the remaining steps.
> See [gpu-sharing.md](./gpu-sharing.md) for the full design, the
> MIG-instance + NVLink-domain inventory, and the phased rollout.

## Real detection (follow-up)

`detectGPUs()` in `gpu.go` is **a static stub returning nil
today**. Intended detection path :

1. Walk `/sys/class/drm/card*/device/` for PCI devices with
   vendor `0x10de` (NVIDIA).
2. Shell out to `nvidia-smi --query-gpu=name,memory.total,
   mig.mode.current --format=csv,noheader,nounits` for each
   card.
3. Map `name` to canonical Model strings (`"H200"`,
   `"RTX-6000-Ada"`) via a static lookup ; unknown SKUs pass
   through verbatim so newer hardware isn't blocked on code.
4. Populate `MIGCapable` from the `mig.mode.current` column.

Detector must be CGo-free (per `coverage_policy`) and degrade
gracefully when nvidia-smi is absent — log + continue with
empty inventory rather than failing registration.

Until that lands, seed `Host.GPUs` statically via
`cluster.hcl`'s `host { gpu { … } }` blocks (HCL schema is in
place). A complementary follow-up wires
`nvidia-container-runtime` so the microVM driver attaches the
selected cards to the guest — today the scheduler **selects**
but doesn't yet **attach**.

## Examples

H200 host (one `gpu` block per card — repeat 8× for a full DGX) :

```hcl
host "gpu-dc-01" {
  hostname = "gpu-dc-01" ; az = "us-east-1a"
  hypervisor = "qemu"    ; architecture = "amd64"
  gpu "H200" { vendor = "nvidia" ; memory_gib = 141 ; mig_capable = true }
  # ... seven more identical blocks for an 8-card chassis ...
}
```

Workstation node with one RTX 6000 Ada :

```hcl
host "wks-01" {
  hostname = "wks-01" ; az = "studio"
  hypervisor = "qemu" ; architecture = "amd64"
  gpu "RTX-6000-Ada" { vendor = "nvidia" ; memory_gib = 48 }
}
```
