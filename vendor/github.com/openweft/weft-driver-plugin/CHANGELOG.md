# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- `go.mod` pins `weft-drivers` v0.1.0 and drops the local
  `replace` directive — the package is now consumed as a regular
  module dependency. Commit `f87e775`.

## [0.2.0]

go-plugin contract surface refresh. See git history for the
RPC-level details.

## [0.1.0] - 2026-05-31

Initial release. go-plugin gRPC contract used by `weft-driver-*`
to talk to `weft agent`. BSD 3-Clause LICENSE (`e375602`).
