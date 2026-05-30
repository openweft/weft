// Package drivers defines the side-effecting interfaces that
// turn weft registry state into actual host-level resources:
// VMs running on a hypervisor, virtio-net devices wired to a
// bridge or WireGuard interface, disk images on a storage
// backend, OCI artifacts in a local cache.
//
// The split between this package and the registries in weft/ is
// deliberate (see [[weft-driver-registry-split]] memory entry):
//
//   * weft/<name>.go owns data + ACL + HCL + Storage. Stays
//     in-process. No side effects.
//   * drivers/<name>.go owns the actual implementation that talks
//     to the kernel / hypervisor / SAN. Designed from day one as
//     a context-aware interface so it can later be swapped for a
//     go-plugin process or a remote weft-agent without touching
//     call sites.
//
// All driver methods:
//
//   * Take `context.Context` for cancellation, deadlines, and
//     trace propagation.
//   * Use protobuf-friendly types only (no in-process Go pointers,
//     no interface-typed values inside spec structs, no maps with
//     non-string keys). This keeps the path to a gRPC plugin
//     boundary clear.
//   * Are stateless from the caller's POV — drivers re-derive
//     everything from the spec they receive. The source of truth
//     stays in the registries.
//   * Are idempotent for "Ensure" / "Attach" operations — calling
//     them again with the same spec is a no-op, not an error.
//     This matters for reconciliation loops.
//
// Multi-host shape (target): one driver instance per (Host, type).
// weft-control's scheduler picks the Host UUID for a workload, then
// dispatches the driver call to that host's agent. Today the
// "agent" is in-process; tomorrow it's gRPC over MTLS.
package drivers
