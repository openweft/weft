//go:build darwin || freebsd

package secretsource

import "golang.org/x/sys/unix"

// SOL_LOCAL is 0 on macOS and FreeBSD; LOCAL_PEERCRED returns the peer's
// effective credentials as of connect().
const solLocal = 0

func peerUID(fd int) (int, error) {
	x, err := unix.GetsockoptXucred(fd, solLocal, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(x.Uid), nil // x is never nil without an error
}
