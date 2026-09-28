# Test coverage

The CI gate (`.github/workflows/ci.yml`) enforces a floor on the **combined**
`-coverpkg=./...` statement profile produced by `task ci` on the native
(amd64/arm64) runners, where the full e2fsprogs oracle is installed.

- **Measured:** ~70.1% (full e2fsprogs run, network image tests skipped — the
  CI condition).
- **Floor:** 69.0% — set ~1% below the observed minimum because a handful of
  journal/allocator tests cover timing-dependent branches, so the total floats
  by a few tenths of a percent run-to-run.

The read/parse path and the common write and error paths are covered. The
residual below 100% is **not** happy-path gaps; it is the four categories
inventoried here. None of it is faked with dead calls or no-op tests.

## Where the corpus comes from

`testdata/fixtures.tar.gz` is a set of real ext2/ext3/ext4 images built by the
e2fsprogs oracle (`testdata/gen.go`, `//go:build ignore`) — varied block sizes,
extents vs. block map, inline data, fast/slow symlinks, an htree directory, a
sparse file, an external xattr, plus a corruption corpus. It is embedded with
`go:embed` so the pure-Go decoder runs on every arch under QEMU (no `mke2fs`
needed at test time), including big-endian s390x, which validates that the
little-endian on-disk structures decode correctly. Regenerate with:

```
go run ./testdata/gen.go
```

## Residual-uncovered inventory (why < 100%)

### A. Deep concurrency / timing-dependent branches (largest share)

The bulk of the uncovered statements are the contended paths of the concurrent
write machinery, reachable only under real lock contention or allocation races
that a deterministic test cannot pin:

- `alloc.go` — `lockedAllocBlocks`, `allocBlocksWithTx`, `allocBlocks`: the
  bounded-retry/backoff loop on per-group BGD locks, the "descriptor stale,
  re-check the bitmap" candidate path, and cross-group fallback under
  contention.
- `commit_dispatch.go` — `enqueueCommitWritesUnderLock`, the dispatcher `run`
  loop, and the queue-full producer-throttle fallbacks of the global sync
  dispatcher.
- `metadata_locks.go` — the reservation/ack lock protocol's contended and
  compound-error branches.
- `journal.go` — replay/commit error branches and the sync-queue fallbacks.

### B. Defensive / rare compound-error branches

`write.go`, `resize.go` (Shrink/Resize rollback), `dir.go`, `mkdir.go`,
`rename.go`, `link.go`, `inline.go` (`systemDataValue` external-xattr overflow):
single-failure error branches are largely covered by the `*_error_test.go`
suites; what remains needs *combinations* of injected failures mid-operation
(e.g. an allocation failure followed by a journal-abort write failure).

### C. Debug/trace instrumentation compiled into the non-`test` build

- `trace_noop.go` (12 no-op hooks) and `trace_nontest.go` (6 no-op impls): the
  default `!test` build compiles these no-ops; the `test`-tag build swaps in the
  instrumented versions. The coverage run does not pass `-tags test`, so the
  no-op bodies show as uncovered.
- `debug_log.go` — `debugPrintf`/`initDebug` are gated on a debug env var that
  is off in CI.

### D. Currently-unwired helpers (dead code — flagged for follow-up)

- `write.go` `writeExtentTree`: a complete multi-extent-tree writer (handles the
  ">4 extents, one level of indirection" case) with **no callers** — the live
  write path only sets inline extents (≤4). It should either be wired in to
  support heavily-fragmented files or removed.
- `journal.go` `startSyncWorkers`/`shutdownSyncWorkers`: explicit
  "kept for compatibility; no-op with global dispatcher" stubs with no callers.

Categories C and D also include the test-only helpers that live in the
production package without a build tag (`test_helpers.go`, `test_wrappers.go`);
several are unused wrappers that inflate the denominator without being
production code.
