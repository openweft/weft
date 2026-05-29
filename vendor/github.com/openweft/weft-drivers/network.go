package drivers

import "context"

// NetworkDriver materialises Networks and Ports on one host.
// Different network types may be served by different drivers
// (a "linux-bridge" driver for nat/bridged/isolated and a
// "wireguard" driver for mesh, or one combined driver) — the
// Adapter routes to the right one based on NetworkSpec.Type.
//
// Lifecycle contract: all Ensure* / Attach* / Detach* methods
// MUST be idempotent. The reconciler invokes them whenever it
// believes the desired state has drifted; spurious retries must
// not break things.
//
// AllocateIP / AllocateMAC are optional helpers — the Adapter
// may choose to manage IP/MAC allocation centrally (in the port
// registry) and pass them in via PortSpec, in which case the
// driver doesn't see Allocate* at all. Drivers that own
// allocation (e.g. integrating with an external IPAM like
// Infoblox) implement them.
type NetworkDriver interface {
	HostInfo(ctx context.Context) (HostInfo, error)

	// EnsureNetwork creates the host-side construct for this
	// network: a bridge for nat/bridged, a WireGuard interface
	// for mesh, nothing visible for isolated. Idempotent.
	EnsureNetwork(ctx context.Context, spec NetworkSpec) error

	// DestroyNetwork tears down the host-side construct. No-op
	// if it never existed.
	DestroyNetwork(ctx context.Context, networkUUID string) error

	// AttachPort wires a VM NIC to the network. Returns the
	// NICHandle the hypervisor needs to plug into its VM config
	// (tap device name, MAC, …).
	AttachPort(ctx context.Context, spec PortSpec) (NICHandle, error)

	// DetachPort tears down the host-side port state.
	DetachPort(ctx context.Context, portUUID string) error

	// RotateMeshPeer updates the WireGuard peer entry for one
	// port without dropping existing connections. Only meaningful
	// for mesh-type networks; drivers serving other types may
	// return ErrNotApplicable.
	RotateMeshPeer(ctx context.Context, spec PortSpec) error
}
