// Package weftdriverplugin is the wire contract that lets weft run its
// hypervisor/network/volume/image drivers as external go-plugin processes
// instead of linking them in-process.
//
// Why this exists: the Apple-VZ driver (weft-driver-vz) binds macOS
// frameworks via cgo, which forced the whole weft binary to be CGO_ENABLED=1
// on darwin. Moving every driver behind a gRPC go-plugin boundary keeps the
// weft core pure-Go (CGO_ENABLED=0 on every platform); the cgo lives only in
// the weft-driver-vz plugin executable, and the virtualization entitlement
// applies to that binary alone.
//
// Shape:
//
//   - The four driver interfaces in github.com/openweft/weft-drivers each map
//     to one gRPC service in driverpb (one RPC per method). They were designed
//     flat + context-aware + sentinel-error'd for exactly this — see that
//     package's doc.go.
//   - One plugin process per host serves all four services on a single
//     connection (BundlePlugin). The host dispenses a *DriverSet of client
//     stubs that satisfy the drivers.* interfaces, so nothing above the
//     HostHandle/DriverHandles boundary changes.
//   - Host side: Launch() locates + starts the plugin and returns the set.
//     Plugin side: a tiny main calls Serve() with its concrete bundle.
//
// The public-contract sentinels (drivers.ErrNotApplicable / ErrUnsupported /
// ErrNotFound) and context cancellation survive the boundary as gRPC status
// codes, reconstructed host-side so errors.Is keeps working (see convert.go).
package weftdriverplugin
