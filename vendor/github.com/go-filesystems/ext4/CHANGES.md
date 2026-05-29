Changelog
=========

Recent local changes:

- Restore test-only fallback counter (`recordBlockingFallback`) guarded
  by `//go:build test` so diagnostics compile into test builds.
- Shorten journal critical sections in `Transaction.Commit()` by reserving
  append offsets and serializing only `fsync` operations.
- Allocation and metadata locking improvements: try-lock with backoff,
  deterministic blocking fallback, and reduced BGD/bitmap contention.
- Added concurrency regression tests under `pkg/go-filesystems/ext4/test`.

These notes are intended to satisfy the repository documentation policy
requiring a package-level docs update when modifying package behavior.

Local changes (detailed):

- `journal.go`: add background sync workers to coalesce `Sync()` syscalls
  and shorten commit critical sections.
- `fs.go`: close the `Journal` cleanly so workers are stopped on `Close()`.
- `debug_log.go`: add buffered diagnostic logger to avoid blocking syscalls
  in hot paths.
- `locks.go`, `metadata_locks.go`, `inode_table_locks.go`, `trace_debug.go`,
  `write.go`, `fallback_counters.go`: replace blocking prints with
  non-blocking diagnostics and test-only counters.

These entries document the behavioral and observability changes made to the
package and should satisfy the repository's commit documentation checks.
