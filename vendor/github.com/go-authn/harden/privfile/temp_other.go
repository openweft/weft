//go:build !windows

package privfile

import "os"

// newTemp makes a new file, mode 0600, under a name nobody chose (O_EXCL).
func newTemp(dir, pattern string) (namedFile, error) { return os.CreateTemp(dir, pattern) }
