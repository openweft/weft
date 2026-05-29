package filesystem_ext4

import (
	"context"
	"sync"
	"sync/atomic"
)

type ownerCtxKey struct{}

var ownerSeq uint64
var ownerGidMu sync.Mutex
var ownerGidMap = map[uint64]uint64{}

// NewOwner returns a new unique owner token for locking purposes.
func NewOwner() uint64 {
	v := atomic.AddUint64(&ownerSeq, 1)
	if v == 0 {
		v = atomic.AddUint64(&ownerSeq, 1)
	}
	// Record the goroutine id that created this owner so Owner() can avoid
	// handing the token to other goroutines (prevents accidental reuse).
	gid := getGID()
	ownerGidMu.Lock()
	ownerGidMap[v] = gid
	ownerGidMu.Unlock()
	return v
}

// WithOwner returns a new context that carries a unique owner token and the token itself.
// Useful for propagating an ownership token through call chains.
func WithOwner(ctx context.Context) (context.Context, uint64) {
	id := NewOwner()
	return context.WithValue(ctx, ownerCtxKey{}, id), id
}

// OwnerFrom extracts an owner token from context.
func OwnerFrom(ctx context.Context) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	v := ctx.Value(ownerCtxKey{})
	if v == nil {
		return 0, false
	}
	id, ok := v.(uint64)
	return id, ok
}

// ownerBelongsToCurrentGoroutine reports whether the provided owner token
// was created in the currently executing goroutine. This helps `Owner()` in
// `reentrantMutex` decide whether it is safe to return an existing token for
// reuse in this goroutine (avoids cross-goroutine reuse that can corrupt
// recursion accounting).
func ownerBelongsToCurrentGoroutine(owner uint64) bool {
	if owner == 0 {
		return false
	}
	gid := getGID()
	ownerGidMu.Lock()
	og, ok := ownerGidMap[owner]
	ownerGidMu.Unlock()
	if !ok {
		return false
	}
	return og == gid
}
