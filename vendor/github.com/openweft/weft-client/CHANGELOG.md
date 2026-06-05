# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.2.2] — 2026-06-02

### Changed

- Bump `weft-proto` to v0.5.0 (pulls in `ListFederationPeers` and the plugin `ListPluginCatalogue` / `ListInstalledPlugins` / `InstallPlugin` RPCs).

## [v0.2.1] — 2026-06-02

### Changed

- Bump `weft-proto` to v0.4.0 (pulls in `SetHostCordoned` RPC + `HostInfo.cordoned` and `StartVMRequest.requested_gpus` / `requested_pci`).

## [v0.2.0] — 2026-06-02

### Changed

- Bump `weft-proto` to v0.3.0 (pulls in `GPURequest`, `PCIPassthroughRequest`, and the `requested_gpus` / `requested_pci` fields on `CreateVMRequest` + `RegisterMicroVMRequest`).

## [v0.1.3] — 2026-05-31

### Changed

- Bump `weft-proto` to v0.2.0 (pulls in VolumeSnapshot RPCs).

## [v0.1.2] — 2026-05-31

### Changed

- Bump `grpc-transports/wireguard` to v0.2.0.

## [v0.1.1] — 2026-05-30

### Changed

- Drop local `../../grpc-transports` and `../weft-proto` replace directives from `go.mod`.

## [v0.1.0] — 2026-05-30

### Added

- Initial `weft-client` module imported from existing tree.
