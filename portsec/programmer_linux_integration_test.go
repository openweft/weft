//go:build linux && integration

// programmer_linux_integration_test.go drives portsec.LinuxReconciler
// against a live Linux kernel. Each test enters a fresh network
// namespace (so the `bridge weft-portsec` table never leaks onto the
// CI host), applies a small set of AntispoofRules, then reads the
// kernel state back via the same github.com/google/nftables API the
// reconciler uses, asserting table + chain + rule counts.
//
// Run with :
//   sudo -E env "PATH=$PATH" go test -tags=integration ./portsec/
//
// CAP_NET_ADMIN is required to create the netns + install the bridge
// nftables table ; the workflow at .github/workflows/integration-linux.yml
// runs this on ubuntu-latest via sudo.

package portsec

import (
	"errors"
	"os"
	"runtime"
	"testing"

	nft "github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// enterFreshNetns pins the calling goroutine to its OS thread (a
// netns is a per-thread property in Linux) and switches it into a
// brand-new empty network namespace. The returned cleanup restores
// the original ns. Without CAP_NET_ADMIN the test is skipped (not
// failed) so that an unprivileged `go test ./...` is still green.
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

// setupVethTap creates a veth pair inside the current netns and
// brings both ends up. The "host" end stands in as the tap interface
// the reconciler will reference. portsec doesn't look the tap up via
// netlink (it just emits an iifname string match in nftables), so
// the rules install regardless ; the veth is mostly here for symmetry
// with the floatingipl2 + portqos tests and to keep the assertions
// honest about a real link being present.
func setupVethTap(t *testing.T, hostName string) {
	t.Helper()
	la := netlink.NewLinkAttrs()
	la.Name = hostName
	la.MTU = 1500
	veth := &netlink.Veth{LinkAttrs: la, PeerName: hostName + "p"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("LinkAdd veth: %v", err)
	}
	host, _ := netlink.LinkByName(hostName)
	peer, _ := netlink.LinkByName(hostName + "p")
	_ = netlink.LinkSetUp(host)
	_ = netlink.LinkSetUp(peer)
}

// findTable returns the bridge-family table the reconciler owns, or
// nil. Used by every assertion below.
func findTable(t *testing.T, c *nft.Conn) *nft.Table {
	t.Helper()
	tables, err := c.ListTablesOfFamily(nft.TableFamilyBridge)
	if err != nil {
		t.Fatalf("ListTablesOfFamily(bridge): %v", err)
	}
	for _, tb := range tables {
		if tb.Name == TableName {
			return tb
		}
	}
	return nil
}

func TestLinuxReconciler_AppliesSingleRule(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()
	setupVethTap(t, "vptap0")

	r := NewLinuxReconciler()
	rules := []AntispoofRule{{
		TapInterface: "vptap0",
		MAC:          "52:54:00:00:00:01",
		IPs:          []string{"10.0.0.5"},
		VMName:       "vm-a",
	}}
	if err := r.Apply(rules); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	c, err := nft.New(nft.AsLasting())
	if err != nil {
		t.Fatalf("nft.New: %v", err)
	}
	defer c.CloseLasting()

	table := findTable(t, c)
	if table == nil {
		t.Fatalf("table %q not installed in bridge family", TableName)
	}

	chains, err := c.ListChainsOfTableFamily(nft.TableFamilyBridge)
	if err != nil {
		t.Fatalf("ListChainsOfTableFamily: %v", err)
	}
	var input *nft.Chain
	for _, ch := range chains {
		if ch.Table != nil && ch.Table.Name == TableName && ch.Name == "input" {
			input = ch
		}
	}
	if input == nil {
		t.Fatalf("input chain not found under %q", TableName)
	}

	got, err := c.GetRules(table, input)
	if err != nil {
		t.Fatalf("GetRules: %v", err)
	}
	// One rule per (mac-drop) plus one per family the rule carries
	// IPs in. Single v4 IP → 2 rules total.
	if want := 2; len(got) != want {
		t.Errorf("rule count = %d, want %d", len(got), want)
	}
}

func TestLinuxReconciler_AppliesMultipleRulesPerTap(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()
	setupVethTap(t, "vptap0")
	setupVethTap(t, "vptap1")

	r := NewLinuxReconciler()
	rules := []AntispoofRule{
		{
			TapInterface: "vptap0",
			MAC:          "52:54:00:00:00:01",
			IPs:          []string{"10.0.0.5", "2001:db8::5"},
			VMName:       "vm-a",
		},
		{
			TapInterface: "vptap1",
			MAC:          "52:54:00:00:00:02",
			IPs:          []string{"10.0.0.6"},
			VMName:       "vm-b",
		},
	}
	if err := r.Apply(rules); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	c, err := nft.New(nft.AsLasting())
	if err != nil {
		t.Fatalf("nft.New: %v", err)
	}
	defer c.CloseLasting()

	table := findTable(t, c)
	if table == nil {
		t.Fatalf("table %q not installed", TableName)
	}

	chains, _ := c.ListChainsOfTableFamily(nft.TableFamilyBridge)
	var input *nft.Chain
	for _, ch := range chains {
		if ch.Table != nil && ch.Table.Name == TableName && ch.Name == "input" {
			input = ch
		}
	}
	if input == nil {
		t.Fatalf("input chain not found")
	}

	got, err := c.GetRules(table, input)
	if err != nil {
		t.Fatalf("GetRules: %v", err)
	}
	// vm-a → 3 rules (mac + v4 + v6) ; vm-b → 2 rules (mac + v4).
	if want := 5; len(got) != want {
		t.Errorf("rule count = %d, want %d", len(got), want)
	}
}

func TestLinuxReconciler_EmptyInputRemovesTable(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()
	setupVethTap(t, "vptap0")

	r := NewLinuxReconciler()
	// First install some rules so the table exists.
	if err := r.Apply([]AntispoofRule{{
		TapInterface: "vptap0",
		MAC:          "52:54:00:00:00:01",
		IPs:          []string{"10.0.0.5"},
	}}); err != nil {
		t.Fatalf("Apply (initial): %v", err)
	}
	c, err := nft.New(nft.AsLasting())
	if err != nil {
		t.Fatalf("nft.New: %v", err)
	}
	if findTable(t, c) == nil {
		c.CloseLasting()
		t.Fatalf("table should exist after seeding apply")
	}
	c.CloseLasting()

	// Now empty apply : reconciler drops the table outright.
	if err := r.Apply(nil); err != nil {
		t.Fatalf("Apply(nil): %v", err)
	}

	c2, err := nft.New(nft.AsLasting())
	if err != nil {
		t.Fatalf("nft.New (post): %v", err)
	}
	defer c2.CloseLasting()
	if t2 := findTable(t, c2); t2 != nil {
		t.Errorf("table %q still present after empty apply", TableName)
	}
}

func TestLinuxReconciler_ApplyReplacesPriorState(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()
	setupVethTap(t, "vptap0")
	setupVethTap(t, "vptap1")
	setupVethTap(t, "vptap2")

	r := NewLinuxReconciler()
	// 3 rules → 3 mac drops + 3 v4 src drops = 6 rules.
	if err := r.Apply([]AntispoofRule{
		{TapInterface: "vptap0", MAC: "52:54:00:00:00:01", IPs: []string{"10.0.0.5"}, VMName: "old-1"},
		{TapInterface: "vptap1", MAC: "52:54:00:00:00:02", IPs: []string{"10.0.0.6"}, VMName: "old-2"},
		{TapInterface: "vptap2", MAC: "52:54:00:00:00:03", IPs: []string{"10.0.0.7"}, VMName: "old-3"},
	}); err != nil {
		t.Fatalf("Apply (first): %v", err)
	}
	// Replace with a single rule (mac only, no IPs → 1 rule).
	if err := r.Apply([]AntispoofRule{
		{TapInterface: "vptap0", MAC: "52:54:00:00:00:99", VMName: "new"},
	}); err != nil {
		t.Fatalf("Apply (replace): %v", err)
	}

	c, err := nft.New(nft.AsLasting())
	if err != nil {
		t.Fatalf("nft.New: %v", err)
	}
	defer c.CloseLasting()
	table := findTable(t, c)
	if table == nil {
		t.Fatalf("table missing after replace")
	}
	chains, _ := c.ListChainsOfTableFamily(nft.TableFamilyBridge)
	var input *nft.Chain
	for _, ch := range chains {
		if ch.Table != nil && ch.Table.Name == TableName && ch.Name == "input" {
			input = ch
		}
	}
	if input == nil {
		t.Fatalf("input chain missing after replace")
	}
	rules, _ := c.GetRules(table, input)
	if want := 1; len(rules) != want {
		t.Errorf("rule count after replace = %d, want %d", len(rules), want)
	}
}
