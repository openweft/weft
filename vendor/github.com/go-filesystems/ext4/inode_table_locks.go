package filesystem_ext4

import (
	"fmt"
	"runtime"
	"sync"
)

var inodeTableGroupLocksMu sync.Mutex
var inodeTableGroupLocks = map[string]*reentrantMutex{}

func inodeTableLockKey(f readerWriterAt, group uint32) string {
	return fmt.Sprintf("%s:inodeTable:g:%d", fileLockKey(f), group)
}

// lockInodeTableGroup acquires a mutex serializing writes that touch the
// inode table for the given backing file and block group. It returns an
// unlock function that must be deferred by the caller.
func lockInodeTableGroup(f readerWriterAt, group uint32) func() {
	key := inodeTableLockKey(f, group)
	inodeTableGroupLocksMu.Lock()
	m, ok := inodeTableGroupLocks[key]
	if !ok {
		m = &reentrantMutex{}
		inodeTableGroupLocks[key] = m
	}
	inodeTableGroupLocksMu.Unlock()
	// Use goroutine id as owner token so the same goroutine may re-acquire
	// the inode-table group lock safely when necessary.
	owner := getGID()
	m.LockOwner(owner)
	if ext4LockDebug {
		buf := make([]byte, 1<<12)
		n := runtime.Stack(buf, false)
		debugPrintf("DEBUG lockInodeTableGroup acquired key=%s owner=%d\n%s\n", key, owner, string(buf[:n]))
	}
	return func() {
		if ext4LockDebug {
			debugPrintf("DEBUG lockInodeTableGroup releasing key=%s owner=%d\n", key, owner)
		}
		m.UnlockOwner(owner)
	}
}
