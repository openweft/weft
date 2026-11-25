//go:build unix

package secretsource

import (
	"fmt"
	"io/fs"
	"syscall"
)

// checkFileOwner refuses a file the calling user does not own. Mode 0600 on a file
// someone else owns says only that they can read it, and root is not let in:
// a root-owned 0600 file cannot be opened by anyone else, and a root process
// has no reason to take a secret from another account's file.
func checkFileOwner(st fs.FileInfo, path string) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot tell who owns it", path)
	}
	if uid := int(sys.Uid); uid != geteuid() {
		return fmt.Errorf("%s: owned by uid %d, want the calling user (uid %d)", path, uid, geteuid())
	}
	return nil
}
