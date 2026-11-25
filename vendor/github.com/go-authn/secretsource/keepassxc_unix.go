//go:build unix

package secretsource

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// getuid is replaced by tests.
var getuid = os.Getuid

// ownedByMe reports whether fi belongs to this user; what it cannot tell, it
// refuses.
func ownedByMe(fi os.FileInfo) bool {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == getuid()
}

// checkSocket refuses a path that is not a socket reached without a symlink, in
// a directory that only this user can write: nothing is sent to a socket
// someone else could have put there or re-pointed (on Linux the fallback
// directory is under /tmp). It is a first filter; the connection itself is then
// checked with checkPeer, because a path can change between the check and the
// dial.
func checkSocket(path string) error {
	if err := checkDir(filepath.Dir(path)); err != nil {
		return err
	}
	st, err := lstat(path)
	if err != nil {
		return err
	}
	if st.mode&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link", path)
	}
	if st.mode&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a socket", path)
	}
	return nil
}

// pathInfo is what Lstat says a path is. It is a value, so that a caller that
// goes on after an error reads a path that is nobody's and nothing, never a nil.
type pathInfo struct {
	mode os.FileMode
	mine bool
}

func lstat(path string) (pathInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return pathInfo{}, err
	}
	return pathInfo{mode: fi.Mode(), mine: ownedByMe(fi)}, nil
}

// checkDir refuses a directory that is a symbolic link, belongs to another user
// or is writable by group or others: what is written there, or read from there,
// could be put or swapped by someone else.
func checkDir(dir string) error {
	d, err := lstat(dir)
	if err != nil {
		return err
	}
	if d.mode&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link", dir)
	}
	if !d.mine {
		return fmt.Errorf("%s belongs to another user", dir)
	}
	if d.mode.Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others", dir)
	}
	return nil
}

// checkPeer refuses a connected Unix socket whose other end is a process of
// another user, as the kernel reports it (SO_PEERCRED, LOCAL_PEERCRED).
func checkPeer(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("%T is not a Unix socket connection", c)
	}
	uid, err := peerOf(uc)
	if err != nil {
		return fmt.Errorf("cannot read the peer's credentials: %w", err)
	}
	if uid != getuid() {
		return fmt.Errorf("the socket's peer belongs to another user")
	}
	return nil
}

func peerOf(uc *net.UnixConn) (int, error) {
	var uid int
	var perr error
	raw, err := uc.SyscallConn()
	if err == nil {
		err = raw.Control(func(fd uintptr) { uid, perr = peerUID(int(fd)) })
	}
	return uid, errors.Join(err, perr)
}
