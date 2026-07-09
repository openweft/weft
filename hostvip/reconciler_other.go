//go:build !linux

package hostvip

// Non-Linux Reconciler stub. macOS / OpenBSD / FreeBSD don't have
// AF_PACKET + netlink ; they need their own implementations (BPF
// raw socket on darwin/*bsd, `route` syscall for the address bind).
// Until those land, the stub returns ErrUnsupported so callers can
// detect + skip the VIP feature gracefully on these platforms.

import "net/netip"

// StubReconciler implements hostvip.Reconciler but every method
// returns ErrUnsupported. Used by tests and as the default when the
// agent runs on a non-Linux host.
type StubReconciler struct{}

// NewStubReconciler returns a hostvip.Reconciler that's safe to wire
// into a Controller but won't actually bind anything.
func NewStubReconciler() *StubReconciler { return &StubReconciler{} }

func (StubReconciler) Bind(_ netip.Prefix, _ string) error         { return ErrUnsupported }
func (StubReconciler) Unbind(_ netip.Prefix, _ string) error       { return ErrUnsupported }
func (StubReconciler) AnnounceGARP(_ netip.Prefix, _ string) error { return ErrUnsupported }

// NewLinuxReconciler exists on the !linux build only so callers that
// reach for it on darwin / *bsd get the stub. The Linux build's
// reconciler_linux.go shadows this with the real implementation.
func NewLinuxReconciler() Reconciler { return NewStubReconciler() }
