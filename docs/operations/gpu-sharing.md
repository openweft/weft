# Sharing a GPU across microVMs

[`gpu-scheduling.md`](./gpu-scheduling.md) covers how the scheduler
*selects* a host that carries the right card. This document covers the
harder problem the fleet now faces : **sharing one physical GPU across
several microVMs safely**, on the canonical hardware — 8× **H200 NVL**
nodes wired as **two NVLink islands of four cards** (2 × NVL4).

It is the design backing the `gpu.go` / `gpu_alloc.go` inventory +
allocation model and the follow-up issues that wire it end to end.

## TL;DR — what changes vs `gpu-scheduling.md`

| Concern | `gpu-scheduling.md` (today) | This document (target) |
|---|---|---|
| GPU axis | a **filter** : two VMs asking `gpu="h200"` both land on one card | a **claimed resource** : a card / MIG slice is held by at most one VM |
| Unit of allocation | the physical card (`GPU.PCIBDF`) | the card **or** a MIG instance (`MIGInstance.UUID`) |
| Topology | AZ / Rack / Host | adds **NVLink domain** so tensor-parallel groups stay inside one NVL4 |
| Guest attach | scheduler selects, driver does **not** attach | `weft-driver-qemu` emits `vfio-pci,host=<BDF>` (whole card) or `vfio-pci,sysfsdev=<mdev>` (MIG slice) |

## Why microVMs don't block GPU passthrough here

A common worry is that microVMs can't do PCI passthrough. That is true
for **Firecracker** (virtio-mmio only, no PCIe by design) — but weft's
Linux path is **QEMU/KVM** (`weft-driver-qemu`), with Cloud Hypervisor
declared as a future `HostInfo.Hypervisor`. Both are VFIO-capable, so
the device — a whole H200 or a single MIG instance — can be handed
*into* the microVM. The Apple VZ driver has no passthrough path and
refuses any non-empty GPU request.

This means the **API-remoting / gRPC-GPU-pool** approach (a daemon that
receives CUDA work over vsock) is **not** weft's sharing mechanism : we
pass the device into the guest, which keeps CUDA semantics native and —
for whole cards in the same NVL4 handed to one VM — preserves NVLink.
A gRPC inference front (Triton / vLLM) is a fine *optional layer on top*
of a MIG slice, not the partition mechanism itself.

## The three gaps this design closes

### 1. No counted allocation (the blocker)

`gpuAxisMatches` and `gpuRequestSatisfied` are **pure filters** : they
answer "does this host carry a matching card?", never "is that card
already taken?". Two microVMs with the same request both schedule onto
one H200 and collide at VFIO bind time. Sharing safely requires a
**claim layer** that tracks `(host, resource) → VM` and refuses a second
claim on a held resource.

This is implemented as `gpuAllocTable` in `gpu_alloc.go` : an exclusive,
in-memory claim table (whole-card claims keyed by PCI BDF, MIG claims by
MIG-instance UUID) plus an exclusivity-aware matcher
`gpuRequestSatisfiedExcl` that counts only **unclaimed** matching
resources. Claims are released when the VM is deprovisioned
(`ReleaseVM`). Persisting the table to etcd (`/weft/gpu/allocations/*`,
mirroring `weft-network`'s `/weft/network/*`) so claims survive an
agent restart is the documented follow-up.

### 2. MIG instances aren't modelled or attached

The inventory modelled physical cards (`GPU.PCIBDF`) but not the **MIG
instances** carved out of an H200 — and a MIG slice is **not** passed
with `vfio-pci,host=<BDF>` ; it needs the mediated-device path
(`vfio-pci,sysfsdev=/sys/bus/mdev/devices/<uuid>`). To slice one H200
into up to seven microVM-attachable units we need both :

- inventory : `MIGInstance{ ParentBDF, Profile, GIID, CIID, UUID,
  MemoryGiB }` carried on each MIG-capable `GPU` — the model added here ;
- attach : `weft-driver-qemu` emitting `sysfsdev=<uuid>` — follow-up.

Like `GPU.PCIBDF`, `MIGInstances` is **runtime-detected** and not
round-tripped through the host-registry HCL (detection repopulates it at
each registration).

### 3. No NVLink / NVL4 topology axis

The scheduler reasons about AZ / Rack / Host but nothing finer. On a
2×NVL4 node a tensor-parallel group of 2–4 GPUs must land **inside one
NVLink island** — spanning the PCIe gap between the two quads collapses
all-reduce bandwidth. Each `GPU` now carries an `NVLinkDomain` label
(e.g. `"nvl4-a"`) ; the multi-GPU affinity rule that keeps a
`count > 1` request inside a single domain is the documented follow-up.

## Target layout per 8×H200 node

```
Quad A (NVLinkDomain "nvl4-a") → 4 H200, MIG off, vfio-pci,host=BDF
        → tensor-parallel / training microVMs (count≤4, same-domain affinity)
Quad B (NVLinkDomain "nvl4-b") → H200 in MIG → mdev → 1 instance = 1 microVM
        → multi-tenant inference, hardware-isolated, claimed per instance
```

MIG mode is per-card : enabling it on quad B does not touch quad A's
NVLink fabric. A whole-card claim and a MIG claim never overlap because
the detector reports a card as *either* a whole-card resource *or* a set
of MIG instances, never both.

## Allocation model

```
allocatable resource ─┬─ whole card   → resource id = GPU.PCIBDF
                      └─ MIG instance → resource id = MIGInstance.UUID

claim = (HostUUID, ResourceID, Kind, VMUUID, Model, CreatedAtUnixNs)
        exclusive : one ResourceID → at most one live claim
```

Matching under exclusivity (`gpuRequestSatisfiedExcl`) :

- **MIG request** (`MIGSlice != ""`) — count unclaimed `MIGInstance`s of
  the requested profile on MIG-capable matching cards ; satisfied when
  `free ≥ Count`.
- **Whole-card request** — count unclaimed matching cards **with a known
  BDF** ; satisfied when `free ≥ Count`. Cards with an empty BDF
  (statically seeded, never detected) are skipped for exclusive
  allocation — the same "seed-vs-detected" boundary `GPU.PCIBDF`'s doc
  comment already calls out. Detection always sets the BDF, so the
  supported production path is unaffected.

## Phased delivery

1. **This PR** — inventory model (`MIGInstance`, `GPU.NVLinkDomain`,
   `GPU.MIGInstances`) + counted-allocation primitive (`gpu_alloc.go`)
   + exclusivity-aware matcher, all pure-Go with tests. The scheduler
   and driver are **not** rewired yet — selection still uses the
   non-exclusive matcher, exactly as `gpu-scheduling.md` documents.
2. **Scheduler wiring** — `ScheduleVM` consults the claim table, claims
   on success, releases on `DeprovisionVM`. etcd persistence of the
   table.
3. **MIG attach** — `weft-driver-qemu` `sysfsdev=<uuid>` path ;
   `detectGPUs` enumerates MIG instances (`nvidia-smi mig -lgi` +
   `/sys/bus/mdev`).
4. **NVLink affinity** — `detectGPUs` fills `NVLinkDomain` from
   `nvidia-smi topo` ; scheduler keeps `count > 1` requests intra-domain.

Each phase has a tracking issue.
