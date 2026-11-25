//go:build unix

package secretsource

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkCredFile is the permission check for a file in $CREDENTIALS_DIRECTORY.
// systemd hands a service whose User= is not root a root-owned file of mode
// 0440 with an ACL entry for that user (measured on systemd 257: `ls -ln`
// shows `-r--r-----+ 1 0 0`), so the group bits of the mode are the ACL mask,
// not a second reader. What must hold: the owner is root or the caller, and
// "other" has no access. The directory is checked for the same by the caller.
func checkCredFile(_ *os.File, st fs.FileInfo, path string) error {
	if uid := int(st.Sys().(*syscall.Stat_t).Uid); uid != 0 && uid != geteuid() {
		return fmt.Errorf("%s: owned by uid %d, want root or the calling user", path, uid)
	}
	if st.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("%s: mode %04o, must not be accessible to others", path, st.Mode().Perm())
	}
	return nil
}
