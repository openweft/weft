//go:build linux

package cowclone

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// cloneCoW clones src → dst via the FICLONE ioctl (reflink), sharing blocks
// until either side is written. It works on reflink-capable filesystems:
//
//   - ZFS — OpenZFS ≥ 2.2 with block cloning enabled (use ≥ 2.2.2: the
//     2.2.0/2.2.1 block-cloning path had a data-corruption bug). src and dst
//     must live in the same pool/dataset, else the kernel returns EXDEV.
//   - btrfs, XFS (reflink=1).
//
// Unlike clonefile(2), FICLONE clones into an already-open dst fd, so dst is
// created here. Clone has already removed any pre-existing dst, so the
// O_CREATE|O_TRUNC open starts from an empty file.
//
// It returns errCoWUnsupported when the filesystem can't share blocks
// (EOPNOTSUPP/ENOTSUP), when src and dst are on different filesystems (EXDEV),
// or when the ioctl is unimplemented (ENOSYS) or rejected by a filesystem that
// signals "unsupported" as EINVAL — so Clone falls back to a byte copy. Other
// errnos (ENOSPC, EACCES, EPERM, …) are surfaced.
func cloneCoW(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	cloneErr := unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if cloneErr == nil {
		// Block sharing is done; closing dst commits it.
		return out.Close()
	}

	// The empty dst we just created must not masquerade as a successful clone:
	// remove it so the byte-copy fallback (or the surfaced error) starts clean.
	out.Close()
	_ = os.Remove(dst)

	switch {
	case errors.Is(cloneErr, unix.EOPNOTSUPP),
		errors.Is(cloneErr, unix.ENOTSUP),
		errors.Is(cloneErr, unix.EXDEV),
		errors.Is(cloneErr, unix.ENOSYS),
		errors.Is(cloneErr, unix.EINVAL):
		return errCoWUnsupported
	default:
		return cloneErr
	}
}
