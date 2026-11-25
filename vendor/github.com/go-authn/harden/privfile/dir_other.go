//go:build !unix

package privfile

import (
	"fmt"
	"os"
)

// checkDir: there is no owner or mode bit to read here. The directory must
// exist and be a directory.
func checkDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

// protect: nothing to do after the write. On Windows the owner-only access
// list was set when the file was made (temp_windows.go); elsewhere there is
// no mode to set.
func protect(file) error { return nil }
