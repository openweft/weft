# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0] - 2026-06-02

### Added

- **`prepare`** : the OCI → boot translation is now exposed as a
  server-side helper instead of being inlined in the boot path.
  Lets `weft agent` materialise a boot image without going through
  the full pod runner. Commit `e2eceb4`.

### Fixed

- **`initbuild`** (real bug) : the cpio writer is now closed on
  every error path. Previously a write error mid-archive left the
  cpio truncated without trailer ; the consumer would then fail to
  parse with a confusing "unexpected EOF" instead of the real
  underlying error. Commit `77e7160`.

### Changed

- `go.mod` tidy / drift fix to keep the module clean against the
  v0.4.0 weft-proto bump.

## [0.1.0] - 2026-05-31

Initial release. microVM rootfs build helpers + OCI puller for
`weft agent`. BSD 3-Clause LICENSE (`e0444e1`).
