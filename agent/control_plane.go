package agent

// control_plane.go defines what weft-agent expects from the
// control plane it talks to. The interface is deliberately
// minimal:
//
//   * RegisterHost — agent → CP at startup. Returns the host
//     UUID the CP knows the agent by. When the agent already
//     carries a persisted UUID it passes the same value; the
//     CP's idempotent re-register accepts.
//   * AttachDrivers — agent → CP right after Register. Pushes
//     the four driver instances into the CP's dispatch table.
//     In-process: pointers to local *builtin.X. gRPC future:
//     server-side stubs that proxy back to the agent.
//   * Heartbeat — agent → CP on a timer. Bumps last-seen so
//     the CP's TTL sweeper doesn't mark the host Down.
//
// Why an interface (not just gRPC out of the gate):
//
//   1. The single-process integration case — weft-control runs
//      a co-located weft-agent inside its own process — needs no
//      network. The CP impl is a thin shim that delegates to
//      weft.Adapter's existing methods.
//   2. Tests use a recording fake (`fakeControlPlane`) to
//      assert what the agent attempted to do.
//   3. gRPC isn't ready yet; the interface puts the agent
//      ahead of that work without blocking on it.
//
// When the gRPC transport lands, a generated client stub will
// implement this interface and the agent's call sites won't
// change.

import (
	"context"
	"errors"
)

// ControlPlane is the weft-control surface area the agent
// touches. Implementations must be safe for concurrent use —
// the agent's heartbeat goroutine + the reconciler may race.
type ControlPlane interface {
	// RegisterHost announces the agent's identity + capabilities.
	// Idempotent: re-registration with the same UUID refreshes
	// AZ / Endpoint / Hypervisor / Architecture / Labels but
	// preserves CreatedAt. The returned `assignedUUID` matches
	// reg.UUID — the CP only differs on first-time registration
	// where reg.UUID was empty.
	RegisterHost(ctx context.Context, reg HostRegistration) (assignedUUID string, err error)
	// AttachDrivers installs the agent's single driver handle in the
	// CP's dispatch table under hostUUID. Replaces any previous
	// entry for that UUID (matches the agent-reconnect flow). Used
	// by single-plugin agents (legacy build-tag default path).
	AttachDrivers(ctx context.Context, hostUUID string, handles DriverHandles) error
	// AttachDriverSet installs a per-kind dispatch set for hostUUID.
	// Used by multi-plugin agents (Options.Drivers non-empty). The
	// CP routes RPCs to the right kind via the host registry's
	// Drivers capability list to map arch → kind. Implementations
	// that don't support multi-plugin may return ErrAttachSetUnsupported ;
	// the agent then falls back to the legacy AttachDrivers path
	// with the primary kind.
	AttachDriverSet(ctx context.Context, hostUUID string, set map[string]DriverHandles) error
	// Heartbeat bumps the host's last-seen timestamp. The CP
	// MAY also flip the host state from Down → Active when the
	// heartbeat arrives.
	Heartbeat(ctx context.Context, hostUUID string) error
}

// ErrAttachSetUnsupported is returned by ControlPlane.AttachDriverSet
// when the implementation only supports the legacy single-handle
// AttachDrivers path. Multi-plugin agents fall back to AttachDrivers
// when they see this.
var ErrAttachSetUnsupported = errors.New("control plane does not support AttachDriverSet (multi-plugin)")
