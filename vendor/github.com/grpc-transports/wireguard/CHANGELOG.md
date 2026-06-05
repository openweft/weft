# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.3.0] — 2026-05-31

### Added

- Public `BringUp` API — interface-only kernel-mode bring-up.

### Changed

- README clarifies that the kernel backend uses the Go netlink library (not shell).

## [v0.2.0] — 2026-05-30

### Added

- Kernel-mode WireGuard backend on Linux (wgctrl + netlink).

### Changed

- README points at GitHub URLs and scrubs legacy vzd/vzc naming.

## [v0.1.0] — 2026-05-30

### Added

- Initial `github.com/grpc-transports/wireguard` module — gRPC-over-WireGuard transport.
