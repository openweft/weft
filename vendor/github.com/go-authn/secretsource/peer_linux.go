//go:build linux

package secretsource

import "golang.org/x/sys/unix"

func peerUID(fd int) (int, error) {
	cr, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cr.Uid), nil
}
