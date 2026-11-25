//go:build !windows

package secretsource

import (
	"fmt"
	"io/fs"
	"os"
)

// checkSecretFile refuses a file that group or others can read or write. The
// mode comes from the open descriptor's stat.
func checkSecretFile(_ *os.File, st fs.FileInfo, path string) error {
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s: mode %04o, must not be readable or writable by group or others", path, st.Mode().Perm())
	}
	return checkFileOwner(st, path)
}
