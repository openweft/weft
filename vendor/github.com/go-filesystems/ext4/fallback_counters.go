//go:build test
// +build test

package filesystem_ext4

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
)

// fallbackCounts stores per-key counters of blocking fallback occurrences.
// Keys are strings like: "fd:6:kind:inode:g:9:block:false".
var fallbackCounts sync.Map // map[string]*uint64

// recordBlockingFallback increments the per-group blocking-fallback counter
// and emits a short DEBUG line so the test logs can be aggregated.
func recordBlockingFallback(kind string, rw readerWriterAt, g uint32, isBlock bool) {
	fd := -1
	if l, ok := rw.(interface{ UnderlyingFile() *os.File }); ok {
		if uf := l.UnderlyingFile(); uf != nil {
			fd = int(uf.Fd())
		}
	}
	key := fmt.Sprintf("fd:%d:%s:g:%d:block:%t", fd, kind, g, isBlock)
	p, _ := fallbackCounts.LoadOrStore(key, new(uint64))
	atomic.AddUint64(p.(*uint64), 1)
	debugPrintf("DEBUG fallbackBlocking key=%s\n", key)
	// Attempt to find the corresponding tryMutex and emit its owner stack
	// to aid in diagnosing which goroutine held the lock.
	// Only bitmap/group fallbacks are tracked here.
	// Try bitmap-group lock first
	lockKey := bitmapLockKey(rw, g, isBlock)
	bitmapGroupLocksMu.Lock()
	if m, ok := bitmapGroupLocks[lockKey]; ok {
		owner := atomic.LoadUint64(&m.owner)
		m.debugMu.Lock()
		stack := append([]byte(nil), m.ownerStack...)
		m.debugMu.Unlock()
		debugPrintf("DEBUG fallbackOwner bitmap key=%s owner=%d\n%s\n", lockKey, owner, string(stack))
	}
	bitmapGroupLocksMu.Unlock()
	// Also try the BGD group lock (owner might be holding descriptor lock)
	bgdKey := bgdGroupLockKey(rw, g)
	bgdGroupLocksMu.Lock()
	if m, ok := bgdGroupLocks[bgdKey]; ok {
		owner := atomic.LoadUint64(&m.owner)
		m.debugMu.Lock()
		stack := append([]byte(nil), m.ownerStack...)
		m.debugMu.Unlock()
		debugPrintf("DEBUG fallbackOwner bgd key=%s owner=%d\n%s\n", bgdKey, owner, string(stack))
	}
	bgdGroupLocksMu.Unlock()

	// For deeper diagnostics, dump all goroutine stacks so we can inspect
	// who is holding other related locks at the moment of fallback.
	// Keep the dump size bounded to avoid overwhelming the debug buffer.
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	debugPrintf("DEBUG fallbackAllGoroutines size=%d\n%s\n", n, string(buf[:n]))
}

// DumpBlockingFallbacks prints a stable, sorted snapshot of recorded counters.
// This can be invoked from test harnesses when desired.
func DumpBlockingFallbacks() {
	var keys []string
	fallbackCounts.Range(func(k, _ interface{}) bool {
		keys = append(keys, k.(string))
		return true
	})
	sort.Strings(keys)
	for _, k := range keys {
		if p, ok := fallbackCounts.Load(k); ok {
			val := atomic.LoadUint64(p.(*uint64))
			fmt.Printf("FALLBACK %s %d\n", k, val)
		}
	}
}
