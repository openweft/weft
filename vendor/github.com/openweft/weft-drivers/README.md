# vzd-drivers

Driver interfaces + types for vzd. This Go module is the only
dependency every driver implementation (Apple VZ, QEMU/KVM,
WireGuard, Ceph, …) needs to import.

The module is intentionally tiny and dependency-free — no imports
beyond the Go standard library — so its semver contract can stay
stable for years.

## Planned extraction

This module currently lives inside the `mock` monorepo to keep
the dev loop fast. The eventual home is its own git repo:

    github.com/cloud-boot/vzd-drivers-api

See the `vzd-one-repo-per-driver` memory entry for the full
multi-repo layout and rationale.

## Adding a new driver type

Each `<x>.go` file at the root of this package declares one
interface (HypervisorDriver, NetworkDriver, VolumeDriver,
ImageDriver). To add a new driver type:

1. Pick a name — singular noun describing what gets driven
   (e.g. `firewall.go` for a per-host nftables/eBPF driver).
2. Define the interface following the existing conventions:
   - Every method takes `context.Context` first.
   - Use only protobuf-friendly types (no pointers, no
     interface-typed fields, string-keyed maps).
   - `Ensure*` / `Attach*` methods are idempotent.
   - Lifecycle `Delete*` returns nil on "already gone".
3. Add or extend a `Spec` struct in `types.go`.
4. Document the contract in the package doc comment.

## Compatibility contract

- Patch / minor releases (v1.x.y) — backward-compatible: methods
  can be added, fields can be added with sensible zero defaults.
- Major releases (v2.0.0) — breaking changes allowed. Drivers
  opt in to the new major version by importing
  `github.com/cloud-boot/vzd-drivers-api/v2`.
- vzd-control supports multiple major versions simultaneously
  via the driver dispatch layer.
