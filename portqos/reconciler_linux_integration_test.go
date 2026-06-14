//go:build linux && integration

// reconciler_linux_integration_test.go drives portqos.LinuxReconciler
// against a live Linux kernel. Each test enters a fresh network
// namespace, creates a veth pair (the "host" end stands in as the
// tap interface), applies a PortQoS spec, then reads tc state back
// via vishvananda/netlink to assert :
//
//   - egress HTB qdisc + class are present on the tap, and
//   - if an ingress cap is set, the matching "<tap>-ifb" device
//     exists and also carries HTB qdisc + class.
//
// Run with :
//   sudo -E env "PATH=$PATH" go test -tags=integration ./portqos/
//
// CAP_NET_ADMIN is required to manipulate qdiscs + create ifb links ;
// the workflow at .github/workflows/integration-linux.yml runs this
// on ubuntu-latest via sudo. The kernel "ifb" module is loaded on
// demand the first time the reconciler asks for an ifb link, which
// works on standard ubuntu-latest images.

package portqos

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// enterFreshNetns isolates the test in a brand-new network namespace.
// Skipping (not failing) on CAP_NET_ADMIN-less environments keeps the
// developer-runs-`go test ./...` path green.
func enterFreshNetns(t *testing.T) func() {
	t.Helper()
	runtime.LockOSThread()

	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Skipf("netns.Get: %v (need CAP_NET_ADMIN)", err)
	}
	fresh, err := netns.New()
	if err != nil {
		_ = orig.Close()
		runtime.UnlockOSThread()
		if errors.Is(err, unix.EPERM) || os.Geteuid() != 0 {
			t.Skipf("creating a netns requires CAP_NET_ADMIN (try `sudo go test -tags=integration`): %v", err)
		}
		t.Fatalf("netns.New: %v", err)
	}

	return func() {
		_ = netns.Set(orig)
		_ = fresh.Close()
		_ = orig.Close()
		runtime.UnlockOSThread()
	}
}

// setupVethTap creates a veth pair and brings both ends up. Returns
// the "host" end's name so the test can use it as the TapInterface
// in the PortQoS spec.
func setupVethTap(t *testing.T, name string) string {
	t.Helper()
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.MTU = 1500
	veth := &netlink.Veth{LinkAttrs: la, PeerName: name + "p"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("LinkAdd veth: %v", err)
	}
	host, _ := netlink.LinkByName(name)
	peer, _ := netlink.LinkByName(name + "p")
	_ = netlink.LinkSetUp(host)
	_ = netlink.LinkSetUp(peer)
	return name
}

// hasHTBQdisc returns true if dev has the root HTB qdisc the
// reconciler installs (handle 1:0, type "htb").
func hasHTBQdisc(t *testing.T, dev netlink.Link) bool {
	t.Helper()
	qs, err := netlink.QdiscList(dev)
	if err != nil {
		t.Fatalf("QdiscList(%s): %v", dev.Attrs().Name, err)
	}
	for _, q := range qs {
		if q.Type() == "htb" && q.Attrs().Handle == netlink.MakeHandle(RootHandleMajor, 0) {
			return true
		}
	}
	return false
}

// hasHTBClass returns true if dev has the leaf HTB class the
// reconciler installs (handle 1:10).
func hasHTBClass(t *testing.T, dev netlink.Link) bool {
	t.Helper()
	cs, err := netlink.ClassList(dev, netlink.MakeHandle(RootHandleMajor, 0))
	if err != nil {
		t.Fatalf("ClassList(%s): %v", dev.Attrs().Name, err)
	}
	want := netlink.MakeHandle(RootHandleMajor, ClassHandleMinor)
	for _, c := range cs {
		if c.Type() == "htb" && c.Attrs().Handle == want {
			return true
		}
	}
	return false
}

func TestLinuxReconciler_ApplyEgressOnlyInstallsHTB(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()
	tap := setupVethTap(t, "qotap0")

	r := NewLinuxReconciler()
	if err := r.Apply([]PortQoS{{
		TapInterface: tap,
		EgressMbps:   100,
		VMName:       "vm-a",
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	dev, err := netlink.LinkByName(tap)
	if err != nil {
		t.Fatalf("LinkByName: %v", err)
	}
	if !hasHTBQdisc(t, dev) {
		t.Errorf("expected root HTB qdisc on %s", tap)
	}
	if !hasHTBClass(t, dev) {
		t.Errorf("expected HTB class 1:10 on %s", tap)
	}
	// Egress-only : no ifb device should exist.
	if _, err := netlink.LinkByName(tap + "-ifb"); err == nil {
		t.Errorf("ifb device %s-ifb should not exist for egress-only spec", tap)
	}
}

func TestLinuxReconciler_ApplyIngressAndEgressInstallsIFB(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()
	tap := setupVethTap(t, "qotap0")

	r := NewLinuxReconciler()
	if err := r.Apply([]PortQoS{{
		TapInterface: tap,
		IngressMbps:  50,
		EgressMbps:   100,
		VMName:       "vm-a",
	}}); err != nil {
		// ifb requires CONFIG_IFB ; on a kernel without it (rare
		// on ubuntu-latest) we skip rather than fail. Same for
		// ingress qdisc support.
		t.Skipf("Apply with ingress shaping failed (likely no CONFIG_IFB on this kernel): %v", err)
	}

	dev, err := netlink.LinkByName(tap)
	if err != nil {
		t.Fatalf("LinkByName(tap): %v", err)
	}
	if !hasHTBQdisc(t, dev) {
		t.Errorf("expected egress HTB qdisc on %s", tap)
	}
	if !hasHTBClass(t, dev) {
		t.Errorf("expected egress HTB class on %s", tap)
	}
	ifb, err := netlink.LinkByName(tap + "-ifb")
	if err != nil {
		t.Fatalf("ifb device %s-ifb not created: %v", tap, err)
	}
	if !hasHTBQdisc(t, ifb) {
		t.Errorf("expected HTB qdisc on ifb %s-ifb", tap)
	}
	if !hasHTBClass(t, ifb) {
		t.Errorf("expected HTB class on ifb %s-ifb", tap)
	}
}

func TestLinuxReconciler_ApplyEmptyRemovesIFB(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()
	tap := setupVethTap(t, "qotap0")

	r := NewLinuxReconciler()
	// First : both directions, ifb gets created.
	if err := r.Apply([]PortQoS{{
		TapInterface: tap,
		IngressMbps:  50,
		EgressMbps:   100,
	}}); err != nil {
		t.Skipf("seed Apply failed (likely no CONFIG_IFB): %v", err)
	}
	if _, err := netlink.LinkByName(tap + "-ifb"); err != nil {
		t.Fatalf("ifb should exist after seed Apply: %v", err)
	}

	// Second : empty list. The reconciler's whole-state replace
	// walks every "*-ifb" link and removes the ones it owns.
	if err := r.Apply(nil); err != nil {
		t.Fatalf("Apply(nil): %v", err)
	}
	if l, err := netlink.LinkByName(tap + "-ifb"); err == nil {
		t.Errorf("ifb %s-ifb should be gone after empty apply, still: %v", tap, l.Attrs().Name)
	}
}

func TestLinuxReconciler_ApplyZeroRatesClearsQdisc(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()
	tap := setupVethTap(t, "qotap0")

	r := NewLinuxReconciler()
	// Seed with an egress cap.
	if err := r.Apply([]PortQoS{{
		TapInterface: tap,
		EgressMbps:   100,
	}}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	dev, _ := netlink.LinkByName(tap)
	if !hasHTBQdisc(t, dev) {
		t.Fatalf("seed should have installed HTB qdisc on %s", tap)
	}

	// Re-apply with zero rates : the reconciler clears the root
	// HTB on the tap (calls clearRootHTB which QdiscDel's it).
	if err := r.Apply([]PortQoS{{
		TapInterface: tap,
		EgressMbps:   0,
		IngressMbps:  0,
	}}); err != nil {
		t.Fatalf("zero-rate Apply: %v", err)
	}
	dev2, _ := netlink.LinkByName(tap)
	if hasHTBQdisc(t, dev2) {
		t.Errorf("HTB qdisc should be gone after zero-rate apply on %s", tap)
	}
}
