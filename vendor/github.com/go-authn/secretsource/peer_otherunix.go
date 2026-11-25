//go:build unix && !linux && !darwin && !freebsd

package secretsource

import "syscall"

// No portable peer credentials here: the answer is this user's own uid, so the
// path checks of checkSocket stay the only filter.
func peerUID(int) (int, error) { return syscall.Getuid(), nil }
