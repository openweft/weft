// Package driverplugins resolves a weft driver plugin executable
// (weft-driver-vz / weft-driver-qemu) to a local path for go-plugin to launch.
//
// Resolution is local-first, OCI-fallback:
//
//  1. A binary already present locally ($WEFT_PLUGIN_DIR, the weft binary's
//     directory, or $PATH) wins — keeps dev + offline + pre-installed hosts
//     working with zero registry access.
//  2. Otherwise the plugin is pulled from an OCI registry (GHCR by default:
//     ghcr.io/openweft/weft-driver-<hv>:<version>), the per-platform binary
//     layer is extracted into a host-local cache, chmod'd +x, and that path is
//     returned. Cached by content digest, with offline tolerance (a prior
//     cached copy is reused if the registry is unreachable).
//
// Config is the file-with-env-override surface: defaults (GHCR + weft version)
// are overlaid by WEFT_DRIVER_* env vars, and — in cluster deploys — by the
// cluster.hcl `drivers {}` block, which `weft up` propagates to agents as those
// same env vars. This package is pure Go and cross-platform (the cgo lives in
// the pulled weft-driver-vz binary, never here).
//
// Packaging convention (see the CI workflows in the driver repos): each plugin
// is published as an OCI image — a multi-arch index whose per-(os,arch)
// manifest carries a single layer that is the raw executable.
package driverplugins
