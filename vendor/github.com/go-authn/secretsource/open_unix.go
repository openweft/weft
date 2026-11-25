//go:build unix

package secretsource

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// openNoFollow opens without blocking and without following a symbolic link at
// the last component: a FIFO nobody writes to would otherwise hold open(2)
// forever, outside any context. The caller checks the descriptor is a regular
// file; O_NONBLOCK changes nothing for one.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

// openSecretFile is openNoFollow for the file: scheme. A link in the last
// component, dangling or not, is refused with a message that says so; the
// kernel does it (ELOOP), so there is no window between a check and the open.
func openSecretFile(path string) (*os.File, error) {
	f, err := openNoFollow(path)
	if errors.Is(err, syscall.ELOOP) {
		// ELOOP also reports a loop among the directories of the path; the
		// message names a symlink only when the last component is one.
		if st, lerr := os.Lstat(path); lerr == nil && st.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: is a symbolic link, which file: does not follow; point at the file it names instead", path)
		}
	}
	return f, err
}
