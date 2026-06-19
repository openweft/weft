package filesystem_ext4

import (
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// reentrantMutex is a simple re-entrant exclusive mutex keyed by goroutine
// so that the same goroutine can lock multiple times safely.
type reentrantMutex struct {
	mu sync.Mutex
	// `owner` and `recursion` are accessed concurrently by different
	// goroutines (fast-path read of owner in Lock). Use atomics to avoid
	// data races while keeping the quick re-entrant check.
	owner     uint64 // accessed via atomic
	recursion int32  // accessed via atomic
	// acquiredAt records the UnixNano timestamp when the mutex was
	// initially acquired (recursion==1). Used only for diagnostics.
	acquiredAt int64 // accessed via atomic
	// waiters counts goroutines currently waiting to acquire the mutex.
	waiters int32 // accessed via atomic
}

// Lock obtains the mutex using the current goroutine id as owner (legacy
// behavior). Prefer using LockOwner with an explicit owner token.
func (r *reentrantMutex) Lock() {
	panic("reentrantMutex: Lock() deprecated; use LockOwner(owner) with an explicit owner token (NewOwner/WithOwner)")
}

// Unlock releases the mutex using the current goroutine id as owner (legacy
// behavior). Prefer using UnlockOwner with an explicit owner token.
func (r *reentrantMutex) Unlock() {
	panic("reentrantMutex: Unlock() deprecated; use UnlockOwner(owner) with an explicit owner token (NewOwner/WithOwner)")
}

// LockOwner obtains the mutex for the provided owner token. This is the
// preferred API for production use since it does not rely on runtime
// internals such as goroutine ids.
func (r *reentrantMutex) LockOwner(owner uint64) {
	if atomic.LoadUint64(&r.owner) == owner {
		n := atomic.AddInt32(&r.recursion, 1)
		if ext4LockDebug {
			debugPrintf("DEBUG LockOwner reenter owner=%d recursion=%d\n", owner, n)
		}
		return
	}
	// Track waiting goroutines for contention diagnostics.
	atomic.AddInt32(&r.waiters, 1)
	startWait := time.Now()
	r.mu.Lock()
	waitNs := time.Since(startWait)
	atomic.AddInt32(&r.waiters, -1)
	atomic.StoreUint64(&r.owner, owner)
	atomic.StoreInt32(&r.recursion, 1)
	atomic.StoreInt64(&r.acquiredAt, time.Now().UnixNano())
	// Optionally warn if wait duration exceeded threshold.
	if ext4LockWaitWarnMs != 0 {
		if waitNs >= time.Duration(ext4LockWaitWarnMs)*time.Millisecond {
			buf := make([]byte, 1<<12)
			n := runtime.Stack(buf, false)
			debugPrintf("WARN reentrantMutex waited owner=%d wait=%dms waiters=%d\nstack:\n%s\n",
				owner, waitNs.Milliseconds(), atomic.LoadInt32(&r.waiters), string(buf[:n]))
		}
	}
	if ext4LockDebug {
		debugPrintf("DEBUG LockOwner acquire owner=%d recursion=1\n", owner)
	}
	// Optionally dump stack on acquire for heavy debugging.
	if ext4LockStackOnAcquire {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("STACK_ON_ACQUIRE owner=%d\n%s\n", owner, string(buf[:n]))
	}
}

// UnlockOwner releases the mutex for the provided owner token.
func (r *reentrantMutex) UnlockOwner(owner uint64) {
	cur := atomic.LoadUint64(&r.owner)
	if cur != owner {
		// Programming error: unlocking from non-owner. Include current
		// owner value to aid diagnosing which token was expected vs found.
		panic(fmt.Sprintf("reentrantMutex: unlock from non-owner (have=%d want=%d)", cur, owner))
	}
	n := atomic.AddInt32(&r.recursion, -1)
	if ext4LockDebug {
		debugPrintf("DEBUG UnlockOwner owner=%d recursion=%d\n", owner, n)
	}
	if n == 0 {
		// Compute hold duration for diagnostics and optionally warn when
		// a lock was held for longer than the configured threshold.
		heldNs := time.Now().UnixNano() - atomic.LoadInt64(&r.acquiredAt)
		atomic.StoreInt64(&r.acquiredAt, 0)
		atomic.StoreUint64(&r.owner, 0)
		r.mu.Unlock()
		if ext4LockHoldWarnMs != 0 {
			if heldNs >= int64(ext4LockHoldWarnMs)*int64(time.Millisecond) {
				buf := make([]byte, 1<<12)
				n := runtime.Stack(buf, false)
				debugPrintf("WARN reentrantMutex held owner=%d duration=%dms\nstack:\n%s\n", owner, heldNs/int64(time.Millisecond), string(buf[:n]))
			}
		}
		if ext4LockDebug {
			debugPrintf("DEBUG UnlockOwner released owner=%d\n", owner)
		}
	}
}

// Owner returns the current owner token (0 if unlocked).
func (r *reentrantMutex) Owner() uint64 {
	o := atomic.LoadUint64(&r.owner)
	// Only return the owner token when it belongs to the current goroutine.
	// This avoids accidentally handing a token created in another goroutine
	// to this goroutine which would allow cross-goroutine reuse and corrupt
	// recursion accounting.
	if ownerBelongsToCurrentGoroutine(o) {
		return o
	}
	return 0
}

var fileLocksMu sync.Mutex
var fileLocks = map[string]*reentrantMutex{}
var inodeLocksMu sync.Mutex
var inodeLocks = map[string]*reentrantMutex{}

// fileLockKey returns a stable key for the provided readerWriterAt so we can
// maintain a lock per backing file/buffer. For *os.File we key by FD; for
// in-memory types we key by pointer address and type.
func fileLockKey(f readerWriterAt) string {
	if f == nil {
		return "nil"
	}
	// If it's a raw *os.File, key by fd.
	if of, ok := f.(*os.File); ok {
		return fmt.Sprintf("fd:%d", of.Fd())
	}

	// If a wrapper exposes the underlying *os.File via a method, use it
	// instead of attempting reflection on unexported fields which can
	// panic under the race detector or from external test packages.
	type underlyingFileGetter interface{ UnderlyingFile() *os.File }
	if h, ok := f.(underlyingFileGetter); ok {
		if of := h.UnderlyingFile(); of != nil {
			return fmt.Sprintf("fd:%d", of.Fd())
		}
	}

	// Fallback: use pointer address when possible, else type name.
	rv2 := reflect.ValueOf(f)
	if rv2.Kind() == reflect.Ptr {
		return fmt.Sprintf("%T:%p", f, unsafe.Pointer(rv2.Pointer()))
	}
	return fmt.Sprintf("%T", f)
}

// getFileLock returns the reentrantMutex associated with the given
// readerWriterAt, creating it if necessary.
func getFileLock(f readerWriterAt) *reentrantMutex {
	key := fileLockKey(f)
	fileLocksMu.Lock()
	m, ok := fileLocks[key]
	if !ok {
		m = &reentrantMutex{}
		fileLocks[key] = m
	}
	fileLocksMu.Unlock()
	return m
}

// inodeLockKey returns a stable key for per-inode locks scoped to the
// backing file/buffer. This allows fine-grained locking per inode number.
func inodeLockKey(f readerWriterAt, inodeNum uint32) string {
	return fmt.Sprintf("%s:ino:%d", fileLockKey(f), inodeNum)
}

// getInodeLock returns a pointer to a RWMutex for the given backing file
// and inode number (creates it lazily).
func getInodeLock(f readerWriterAt, inodeNum uint32) *reentrantMutex {
	key := inodeLockKey(f, inodeNum)
	inodeLocksMu.Lock()
	l, ok := inodeLocks[key]
	if !ok {
		l = &reentrantMutex{}
		inodeLocks[key] = l
	}
	inodeLocksMu.Unlock()
	return l
}

var dirModLocksMu sync.Mutex
var dirModLocks = map[string]*sync.Mutex{}

// getDirModLock returns a plain (non-reentrant) mutex that serialises the
// entire "read-directory-block → insert-entry → commit-transaction" sequence
// for a given parent directory inode and backing file.  The mutex must be
// held from the point the caller first reads the directory's entry list until
// after the transaction that adds the new entry has been committed to disk.
// Without this serialisation, two concurrent WriteFile calls for the same
// parent directory can both read the same directory block, both insert their
// entry into their respective (independent) transactions, and then commit in
// sequence — the second commit overwrites the first commit's directory block,
// losing the first file's directory entry (classic lost-update).
func getDirModLock(f readerWriterAt, dirIno uint32) *sync.Mutex {
	key := fmt.Sprintf("%s:dirmod:%d", fileLockKey(f), dirIno)
	dirModLocksMu.Lock()
	m, ok := dirModLocks[key]
	if !ok {
		m = &sync.Mutex{}
		dirModLocks[key] = m
	}
	dirModLocksMu.Unlock()
	return m
}

// getGID returns the numeric goroutine id parsed from runtime.Stack. This is
// a hack used for re-entrant lock ownership. It is on the lock hot path, so it
// parses the id straight out of the stack-allocated buffer ("goroutine NNN
// [...") without allocating — no string conversion, strings.Fields, or
// strconv (which together were ~a third of all allocations during writes).
func getGID() uint64 {
	var buf [32]byte
	n := runtime.Stack(buf[:], false)
	b := buf[:n]
	const prefix = "goroutine "
	if len(b) < len(prefix) {
		return 0
	}
	b = b[len(prefix):]
	var id uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}
