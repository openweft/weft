//go:build test
// +build test

package filesystem_ext4

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	commitTraceMu  sync.Mutex
	commitTraceBuf []string
)

func commitTraceEnabled() bool {
	return os.Getenv("EXT4_COMMIT_TRACE") == "1"
}

func commitTraceEvent(ev string) {
	if !commitTraceEnabled() {
		return
	}
	// Build the string outside the lock to minimize hold time.
	s := fmt.Sprintf("COMMITTRACE %s %s", ev, time.Now().Format(time.RFC3339Nano))
	commitTraceMu.Lock()
	commitTraceBuf = append(commitTraceBuf, s)
	commitTraceMu.Unlock()
}

func commitTraceDuration(ev string, start time.Time) {
	if !commitTraceEnabled() {
		return
	}
	d := time.Since(start)
	// Compute string before locking to keep critical section small.
	s := fmt.Sprintf("COMMITTRACE %s %s", ev, d)
	commitTraceMu.Lock()
	commitTraceBuf = append(commitTraceBuf, s)
	commitTraceMu.Unlock()
}

// DumpCommitTrace prints accumulated commit traces to stdout.
func DumpCommitTrace() {
	commitTraceMu.Lock()
	defer commitTraceMu.Unlock()
	for _, s := range commitTraceBuf {
		fmt.Println(s)
	}
}
