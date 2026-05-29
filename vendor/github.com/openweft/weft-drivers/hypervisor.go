package drivers

import "context"

// HypervisorDriver materialises a VMSpec into a running VM on
// one specific compute host. One driver instance per (host,
// hypervisor type): Apple VZ on macOS, QEMU/KVM on Linux, Cloud
// Hypervisor on Linux, …
//
// Lifecycle contract:
//
//   CreateVM   → idempotent: re-calling with same UUID is no-op
//   StartVM    → idempotent: already-running is no-op
//   StopVM     → idempotent: already-stopped is no-op
//   DeleteVM   → idempotent: missing is no-op (not an error)
//   AttachDisk → idempotent on (vmUUID, disk.VolumeUUID)
//   DetachDisk → idempotent: missing attachment is no-op
//   AttachNIC  / DetachNIC follow the same contract
//
// Idempotence matters because the scheduler / reconciler may
// retry on transient failures (network blip, transient lock
// contention). Drivers that turn retries into errors create
// unfixable stuck states.
//
// Errors:
//
//   * Return a concrete error for "this can never succeed" (bad
//     spec, hypervisor missing on host, hardware fault).
//   * Return a wrapped context error for cancellation / timeout
//     — callers expect `errors.Is(err, ctx.Err())` to work.
//   * NEVER panic across the interface boundary; the gRPC plugin
//     transport can't surface them cleanly.
type HypervisorDriver interface {
	HostInfo(ctx context.Context) (HostInfo, error)

	// CreateVM provisions the VM's static state (nvram, machine
	// identifier, base config). Disks + NICs come via Attach*.
	CreateVM(ctx context.Context, spec VMSpec) error

	// StartVM boots the VM. The driver returns once the
	// hypervisor reports "running" (or the equivalent).
	StartVM(ctx context.Context, vmUUID string) error

	// StopVM signals graceful shutdown. After ctx deadline,
	// drivers may escalate to a hard stop — caller controls the
	// deadline.
	StopVM(ctx context.Context, vmUUID string) error

	// DeleteVM removes every host-local state for the VM.
	// Returns nil for "already gone".
	DeleteVM(ctx context.Context, vmUUID string) error

	AttachDisk(ctx context.Context, vmUUID string, disk DiskSpec) error
	DetachDisk(ctx context.Context, vmUUID, volumeUUID string) error

	AttachNIC(ctx context.Context, vmUUID string, nic NICHandle) error
	DetachNIC(ctx context.Context, vmUUID, nicDevice string) error
}
