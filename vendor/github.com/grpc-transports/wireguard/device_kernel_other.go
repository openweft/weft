//go:build !linux

package wgtransport

import (
	"fmt"
	"log"
	"net/netip"
	"runtime"
)

// bringUpKernel — non-Linux build stub. Returns a clear error so a caller
// that asked for BackendKernel on macOS/Windows sees the platform mismatch
// at startup rather than a confusing netlink syscall failure.
func bringUpKernel(
	_ string,
	_ []byte,
	_ netip.Addr,
	_ uint16,
	_ []Peer,
	_ int,
	_ *log.Logger,
) (wgNet, error) {
	return nil, fmt.Errorf("kernel backend requires Linux (running %s); use BackendUserspace instead", runtime.GOOS)
}
