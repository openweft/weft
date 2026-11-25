//go:build unix

package privfile

import (
	"fmt"
	"os"
	"syscall"
)

// getuid is replaced by tests.
var getuid = os.Getuid

// checkDir refuses a directory that is a symbolic link, belongs to another user
// or is writable by group or others: what is written there could be put or
// swapped by someone else. (secretsource's checkDir, which this is the home of.)
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	// What cannot be read as an owner is refused, not assumed to be ours.
	if sys, ok := fi.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != getuid() {
		return fmt.Errorf("%s belongs to another user", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others", dir)
	}
	return nil
}

// protect makes the open file owner-only.
func protect(f file) error { return f.Chmod(Mode) }
