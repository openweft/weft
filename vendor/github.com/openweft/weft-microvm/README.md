# weft-microvm

Host-side runtime library for **microVMs** — the Docker-style path where an OCI
image is pulled, its rootfs extracted, and booted as a lightweight VM sharing a
common `weft-microvm-init` kernel over virtio-fs. This is the runtime behind the
`weft microvm` CLI; it is **not** the classic full-VM path (boot.iso + cloud-init).

Extracted from the former standalone `weft-microvm` runner (weft-microvm) so the
logic lives in the openweft org and is consumed directly by the single `weft`
binary — there is no separate `weft-microvm` binary anymore.

## Module

```
github.com/openweft/weft-microvm
```

## Model (Docker-like, not classic VM)

- `Pull` an OCI image → rootfs extracted to `$XDG_DATA_HOME/weft-microvm/images/<refsafe>/rootfs/`,
  with `.weft-microvm/config.json` materialised from the image config.
- `Run` boots a microVM by dialing the weft agent's gRPC API and calling
  `RegisterMicroVM`: the rootfs is shared into the guest over **virtio-fs**
  (mount tag `rootfs0`); the kernel/init is the **shared** `weft-microvm-init` UKI, not a
  per-VM boot ISO; no per-VM cloud-init seed.
- Lifecycle verbs (list / rm / logs) are thin gRPC clients and live in the
  `weft microvm` cobra command, not in this library.

## API

```go
// Run prepares the rootfs and registers+starts the microVM via the weft agent.
// Dispatches to the pod path when Args.Pod != "".
func Run(args Args) error

// RunPod boots a multi-container pod manifest (weft-init as PID 1).
func RunPod(args Args) error

// Pull caches an OCI image: pull + extract rootfs + write .weft-microvm/config.json.
func Pull(image string) error

type Args struct {
	Image     string   // OCI ref, e.g. "alpine:3.21"
	Cmd       []string // entrypoint override (the `-- CMD…` tail)
	Detach    bool
	MountTag  string   // virtio-fs tag (default "rootfs0")
	WeftSocket string   // weft agent gRPC socket (default ~/.weft/weft.sock)
	Project   string   // tenant project namespace
	Pod       string   // path to a pod manifest JSON (pod mode)
}
```

Sub-package `initbuild`:

```go
// PackToFile packs a Linux weft-microvm-init ELF into an initramfs cpio.gz.
func PackToFile(initBinary, dst string) error
```

## Used by

- [`openweft/weft`](../weft) `cmd/weft/microvm` — the `weft microvm` command group
  (`run` / `pull` / `ls` / `rm` / `logs` / `init-build`).

## Related

- [`openweft/weft-microvm-init`](../weft-microvm-init) — guest-side `weft-microvm-init` / pod supervisor.
- [`cloud-boot/init`](../../cloud-boot/init) — `pkg/oci` (OCI pull) + `pkg/cpio` (initramfs).
- [`openweft/weft-proto`](../weft-proto) — `RegisterMicroVM` gRPC contract.
