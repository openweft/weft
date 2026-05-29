package filesystem_ext4

import (
	"os"
	"strconv"
)

// Cached lock/diagnostic flags read once at package init to avoid calling
// os.Getenv from hot code paths (which can acquire global locks under the
// test harness and lead to lock-order inversions / contention).
var (
	ext4LockDebug          = os.Getenv("EXT4_LOCK_DEBUG") == "1"
	ext4LockStackOnAcquire = os.Getenv("EXT4_LOCK_STACK_ON_ACQUIRE") == "1"
	ext4LockWaitWarnMs     = func() int {
		if v := os.Getenv("EXT4_LOCK_WAIT_WARN_MS"); v != "" {
			if ms, err := strconv.Atoi(v); err == nil {
				return ms
			}
		}
		return 0
	}()
	ext4LockHoldWarnMs = func() int {
		if v := os.Getenv("EXT4_LOCK_HOLD_WARN_MS"); v != "" {
			if ms, err := strconv.Atoi(v); err == nil {
				return ms
			}
		}
		return 0
	}()
)
