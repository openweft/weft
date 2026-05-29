//go:build darwin

package cowclone

import (
	"errors"

	"golang.org/x/sys/unix"
)

// cloneCoW clones src → dst via APFS clonefile(2), which shares blocks until
// either side is written.
//
// It returns errCoWUnsupported when the host volume isn't APFS (ENOTSUP) or
// when src and dst are on different volumes (EXDEV), so Clone falls back to a
// byte copy. Clone has already removed any pre-existing dst (clonefile fails if
// dst exists).
func cloneCoW(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EXDEV):
		return errCoWUnsupported
	default:
		return err
	}
}
