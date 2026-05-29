# weft

Single binary, HashiCorp-style: `weft agent` boots the long-lived control-plane daemon; `weft <noun> <verb>` issues client RPCs against a running agent. The daemon manages virtual machines via Apple `Virtualization.framework` and exposes a gRPC API over a Unix socket.

Build requirements: **macOS only** (`darwin && cgo`); the binary must be code-signed with the `com.apple.security.virtualization` entitlement.

## Overview

The `weft agent` daemon is the privileged component of the stack. It:

- Manages the full VM lifecycle (create / start / stop / delete)
- Serves a gRPC API over a Unix socket (`~/.vzd/vzd.sock`)
- Optionally exposes the same API over an SSH-secured socket (`~/.vzd/vzd-ssh.sock`) via [`ssh`](../../grpc-transports/ssh)
- Runs all-in-one by default (server + local driver dispatch); `--server` / `--client` split it into control-plane and per-host roles
- Caches OCI and HTTP disk images locally (`imagestore`)
- Injects cloud-init ISOs for SSH key provisioning
- Forks a child process (`vz-vm-run`) for each graphical VM window

## Architecture

```
weft agent (daemon)
├── gRPC server (Unix socket)               ← consumed by weft <noun> clients, weft-ui, Terraform provider
├── gRPC server (SSH-secured Unix socket)   ← consumed over SSH
├── imagestore — OCI/HTTP disk image cache
└── vz-vm-run — forked per running VM (AppKit window)
```

## `Adapter` API

```go
func New(stateDir string) *Adapter
func (a *Adapter) SetPaths(cachePath, vmsPath string)
func (a *Adapter) Pull(ctx context.Context, urls []string, parallel int) error
func (a *Adapter) CloneVM(image, name string, cfg *VMConfig, w io.Writer) error
func (a *Adapter) StartVM(name, cloudInitISO string) error
func (a *Adapter) StopVM(name string) error
func (a *Adapter) DeleteVM(name string) error
func (a *Adapter) ListLocal() (map[string]map[string]interface{}, error)
func (a *Adapter) IP(name string) (string, error)
```

## APFS clonefile for VM disks

`CloneVM` materialises `<vmDir>/disk.img` via macOS `clonefile(2)`
(copy-on-write) on APFS, so creating a VM from a cached image is
O(metadata) regardless of disk size. A 3 GiB Debian raw image
clones in **~500 µs** in practice.

Cache layout to support this:

```text
<cacheDir>/<refsafe>/
  <original-file>          # the pulled HTTP/OCI artefact (qcow2, .raw, OCI blobs…)
  raw.img                  # lazily materialised raw form, used as the clone source
  raw.img.tmp              # transient — atomically renamed to raw.img on success
```

Per-source rules:

- **HTTP raw**: the cached file is already raw, so we `clonefile()`
  straight from it. `raw.img` is never created.
- **HTTP qcow2**: on first `CopyImageToDisk`, `ConvertToRaw` writes
  `raw.img.tmp`, renamed to `raw.img`. Subsequent clones go
  straight to clonefile.
- **OCI (tart)**: same pattern — `ExtractDisk` (LZFSE → raw) lands
  in `raw.img.tmp`, renamed atomically.

A per-image `sync.Mutex` (`rawMaterialiseLocks` keyed by ref)
serialises concurrent `CopyImageToDisk` calls so two parallel
`CloneVM` invocations don't both pay the transcode cost.

Filesystem fallback: if the host isn't APFS (or src/dst are on
different volumes), `clonefile(2)` returns `ENOTSUP`/`EXDEV` and
we drop to a streaming byte copy with identical semantics. The
same primitive is used by `vz-provision --copy` (`placeArtefact`)
for ad-hoc boot-artefact staging.

The microVM path (`RegisterMicroVM` with `share.Clone = true`)
uses the same `clonefile(2)` syscall — there the source is a
*directory* (the OCI rootfs cache); here it's a *file*
(`raw.img`). Both are one syscall each.

## Subcommands

| Subcommand        | Role |
| ----------------- | ---- |
| `weft agent`      | Run the daemon — listen on `--socket`, serve gRPC, manage VMs. `--server` / `--client` select the control-plane vs per-host role; default is all-in-one. |
| `weft up`         | Day-0 cluster bring-up from a `cluster.hcl`: converge the control plane + infra micro-VMs onto 1 host (single-node) or 3 hosts (3-DC), over SSH. Convergent — re-run after adding hosts to grow 1 → 3 in place. See [`cluster/`](cluster/). |
| `weft infra deploy|bootstrap` | Per-host primitive `weft up` composes: deploy the infra micro-VMs (etcd/dex/zot/nats/coredns) from `infra/*/plan.hcl`, in `depends_on` order, in-process. Also a dev escape hatch. |
| `weft <noun> <verb>` | Client RPCs against a running agent (`weft instance list`, `weft image pull …`, `weft host ls`, …). |
| `weft vz-vm-run`   | Hidden subcommand forked per VM by `Adapter.StartVM`; owns the AppKit window + the `VZVirtualMachine` lifecycle. |
| `weft vz-provision` | Lay out a vmDir from pre-built artefacts (boot disk + extra data disks + cloud-init ISO) without going through the OCI imagestore / `CloneVM` path. Useful for ad-hoc tests that already have a boot ISO + cloud rootfs on disk (e.g. cloud-boot's `boot.iso` + a labelled Debian cloud raw). Use `--copy` rather than the default symlink mode — Apple `Virtualization.framework` does not follow symlinks in `NewDiskImageStorageDeviceAttachment`. |

## Run

After `task build` (which compiles + code-signs `./bin/weft`):

```sh
./bin/weft agent --config-dir .mock/hcl      # or: task run
```

All-in-one mono-host default — file storage + in-process event bus, no etcd/NATS required. Then drive it from another terminal: `./bin/weft instance list`, `./bin/weft image pull …`.

| `agent` flag | Default | Description |
|------|---------|-------------|
| `--config-dir` | `.mock/hcl` | HCL config directory (optional — pre-declares VMs/images) |
| `--socket` | `~/.vzd/vzd.sock` | Unix socket path |
| `--ssh-socket` | `~/.vzd/vzd-ssh.sock` | SSH-secured gRPC socket (empty to disable) |
| `--ssh-authorized-keys` | `~/.vzd/authorized_keys` | Path to authorized_keys |
| `--storage-backend` | `file` | `file` (dev) or `etcd` (prod 3-DC cluster) |
| `--event-bus` | `local` | `local` (dev) or `nats` (prod cluster) |
| `--server` / `--client` | (all-in-one) | Split into control-plane-only / per-host-only roles |

## Build

From the **weft repo root**:

```sh
CGO_LDFLAGS="-Xlinker -no_warn_duplicate_libraries" \
  go build -mod=vendor -gcflags "github.com/progrium/darwinkit/...=-N -l" \
  -o bin/weft ./cmd/weft
codesign --entitlements vz.entitlements -s - bin/weft
```

## microVMs vs classic VMs

Two distinct models, both driven by `weft <noun>`:

- **`weft instance …`** — classic full VMs (boot disk + cloud-init, EFI boot via the Apple-VZ driver).
- **`weft microvm …`** — Docker-style microVMs: `weft microvm pull <oci-image>` then `weft microvm run <oci-image>`; the rootfs is shared over virtio-fs and booted on a shared `ncl-init` kernel (no per-VM boot.iso/cloud-init). Runtime logic lives in [`weft-microvm`](../weft-microvm); this replaces the former standalone `ncl` binary.

## Related

- [`weft-proto`](../weft-proto) — gRPC service definition
- [`vzc`](../vzc) — CLI client
- [`weft-ui`](../weft-ui) — graphical dashboard
- [`weft-microvm`](../weft-microvm) — microVM runtime (`weft microvm`)
- [`cloud-init`](../cloud-init) — cloud-init ISO generation
- [`ssh`](../../grpc-transports/ssh) — SSH transport
- [`diskimage`](../../go-diskimages/diskimage) — disk image toolkit
