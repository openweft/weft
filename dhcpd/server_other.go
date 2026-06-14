//go:build !linux

package dhcpd

import (
	"errors"
	"log/slog"
	"net/netip"
)

// LinuxServer on non-linux platforms is a build-stub : every
// method either returns "linux only" or no-ops, so darwin dev
// builds compile cleanly. Callers should branch at construction
// time to NewStub() on non-linux ; this stub exists so the
// import surface (the type name) is consistent across platforms.
type LinuxServer struct {
	opts Options
}

// NewLinuxServer on darwin / windows / *bsd returns an error
// immediately — the real DHCPv4 server only ships on Linux because
// SO_BINDTODEVICE + the BPF-friendly raw paths it leans on don't
// exist elsewhere.
//
// The constructor still validates Options so a caller building the
// config off Linux gets the same validation feedback they would on
// Linux ; only the "linux only" error suppresses the actual bind.
func NewLinuxServer(opts Options) (*LinuxServer, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return nil, errors.New("dhcpd: LinuxServer is linux-only — use NewStub on this platform")
}

// SetLogger is a no-op on non-linux ; the constructor returns nil
// so this method is never actually reached, but the symbol has to
// exist for cross-platform callers that compile against the type.
func (s *LinuxServer) SetLogger(_ *slog.Logger) {}

// Run is also unreachable in practice — included only so the
// (*LinuxServer) value satisfies the Server interface on every
// platform.
func (s *LinuxServer) Run(_ contextLike) error {
	return errors.New("dhcpd.LinuxServer.Run: linux only")
}

// ServerIP returns the configured server identifier ; harmless to
// expose on darwin too.
func (s *LinuxServer) ServerIP() netip.Addr { return s.opts.ServerIP }
