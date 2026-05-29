//go:build !test
// +build !test

package filesystem_ext4

import "time"

// Non-test no-op implementations to satisfy references from non-test builds.
// These functions are defined with full behavior in test-only files when the
// "test" build tag is enabled; here we provide lightweight stubs so regular
// builds do not require the test helpers.

func recordBlockingFallback(kind string, rw readerWriterAt, g uint32, isBlock bool) {}

func DumpBlockingFallbacks() {}

func commitTraceEnabled() bool { return false }

func commitTraceEvent(ev string) {}

func commitTraceDuration(ev string, start time.Time) {}

func DumpCommitTrace() {}
