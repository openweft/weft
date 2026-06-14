//go:build linux && integration

// reconciler_linux_integration_test.go drives the real netlink path
// against a live Linux kernel. The test isolates itself inside a
// fresh network namespace (so the nftables ruleset it installs never
// leaks onto the CI host) and then reads the table back via the same
// github.com/google/nftables API to assert that DNAT/SNAT rules
// landed in the expected chains.
//
// Run with :
//   sudo -E env "PATH=$PATH" go test -tags=integration ./floatingipnat/
//
// CAP_NET_ADMIN is required to create the netns + program nftables ;
// on GitHub Actions ubuntu-latest the workflow uses `sudo`. Locally
// you can also `unshare -rn` and skip sudo if user-namespaces are
// permitted (the GitHub runners disable unprivileged user-ns by
// default, which is why we go through sudo there).

package floatingipnat

import (
	"errors"
	"os"
	"runtime"
	"testing"

	nft "github.com/google/nftables"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// enterFreshNetns pins the calling goroutine to its OS thread (a
// netns is a per-thread property in Linux) and switches it into a
// brand-new empty network namespace. The cleanup func restores the
// original ns. Failing to obtain CAP_NET_ADMIN skips the test —
// useful when developers run `go test` without sudo.
func enterFreshNetns(t *testing.T) func() {
	t.Helper()
	runtime.LockOSThread()

	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("netns.Get: %v", err)
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
		// Best-effort restore : the test goroutine is dying anyway,
		// but a clean Setns lets future tests on this thread see
		// the original ns.
		_ = netns.Set(orig)
		_ = fresh.Close()
		_ = orig.Close()
		runtime.UnlockOSThread()
	}
}

func TestLinuxReconciler_AppliesIPv4Mappings(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()

	r := NewLinuxReconciler()
	mappings := []NATMapping{
		{PublicIP: "203.0.113.5", PrivateIP: "10.0.0.5", VMName: "web"},
		{PublicIP: "203.0.113.6", PrivateIP: "10.0.0.6", VMName: "api"},
	}
	if err := r.Apply(mappings); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	c, err := nft.New(nft.AsLasting())
	if err != nil {
		t.Fatalf("nft.New: %v", err)
	}
	defer c.CloseLasting()

	tables, err := c.ListTablesOfFamily(nft.TableFamilyIPv4)
	if err != nil {
		t.Fatalf("ListTablesOfFamily: %v", err)
	}
	var fipTable *nft.Table
	for _, tb := range tables {
		if tb.Name == natTableName {
			fipTable = tb
			break
		}
	}
	if fipTable == nil {
		t.Fatalf("expected table %q in ipv4 family, got %d tables", natTableName, len(tables))
	}

	chains, err := c.ListChainsOfTableFamily(nft.TableFamilyIPv4)
	if err != nil {
		t.Fatalf("ListChainsOfTableFamily: %v", err)
	}
	var pre, post *nft.Chain
	for _, ch := range chains {
		if ch.Table == nil || ch.Table.Name != natTableName {
			continue
		}
		switch ch.Name {
		case "prerouting":
			pre = ch
		case "postrouting":
			post = ch
		}
	}
	if pre == nil || post == nil {
		t.Fatalf("expected prerouting + postrouting chains, got pre=%v post=%v", pre, post)
	}

	preRules, err := c.GetRules(fipTable, pre)
	if err != nil {
		t.Fatalf("GetRules(prerouting): %v", err)
	}
	postRules, err := c.GetRules(fipTable, post)
	if err != nil {
		t.Fatalf("GetRules(postrouting): %v", err)
	}
	if got, want := len(preRules), len(mappings); got != want {
		t.Errorf("prerouting rule count = %d, want %d", got, want)
	}
	if got, want := len(postRules), len(mappings); got != want {
		t.Errorf("postrouting rule count = %d, want %d", got, want)
	}

	// Each rule's UserData should carry the operator-readable
	// comment with the VM name + dnat/snat kind. We just check
	// the kind tag appears in *some* rule of the right chain.
	if !anyRuleHas(preRules, "dnat") {
		t.Errorf("no prerouting rule carries the dnat comment ; UserData=%v", dumpUserData(preRules))
	}
	if !anyRuleHas(postRules, "snat") {
		t.Errorf("no postrouting rule carries the snat comment ; UserData=%v", dumpUserData(postRules))
	}
	if !anyRuleHas(preRules, "web") || !anyRuleHas(preRules, "api") {
		t.Errorf("prerouting rule comments missing a vm name : %v", dumpUserData(preRules))
	}
}

func TestLinuxReconciler_EmptyMappingsLeavesEmptyTable(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()

	r := NewLinuxReconciler()
	if err := r.Apply(nil); err != nil {
		t.Fatalf("Apply(nil): %v", err)
	}

	c, err := nft.New(nft.AsLasting())
	if err != nil {
		t.Fatalf("nft.New: %v", err)
	}
	defer c.CloseLasting()

	tables, err := c.ListTablesOfFamily(nft.TableFamilyIPv4)
	if err != nil {
		t.Fatalf("ListTablesOfFamily: %v", err)
	}
	var fipTable *nft.Table
	for _, tb := range tables {
		if tb.Name == natTableName {
			fipTable = tb
			break
		}
	}
	if fipTable == nil {
		t.Fatalf("expected empty %q table to still be installed", natTableName)
	}

	chains, err := c.ListChainsOfTableFamily(nft.TableFamilyIPv4)
	if err != nil {
		t.Fatalf("ListChainsOfTableFamily: %v", err)
	}
	for _, ch := range chains {
		if ch.Table == nil || ch.Table.Name != natTableName {
			continue
		}
		rules, err := c.GetRules(fipTable, ch)
		if err != nil {
			t.Fatalf("GetRules(%s): %v", ch.Name, err)
		}
		if len(rules) != 0 {
			t.Errorf("chain %s has %d rules in an empty apply, want 0", ch.Name, len(rules))
		}
	}
}

func TestLinuxReconciler_ApplyReplacesPriorState(t *testing.T) {
	cleanup := enterFreshNetns(t)
	defer cleanup()

	r := NewLinuxReconciler()
	if err := r.Apply([]NATMapping{
		{PublicIP: "203.0.113.5", PrivateIP: "10.0.0.5", VMName: "old-1"},
		{PublicIP: "203.0.113.6", PrivateIP: "10.0.0.6", VMName: "old-2"},
		{PublicIP: "203.0.113.7", PrivateIP: "10.0.0.7", VMName: "old-3"},
	}); err != nil {
		t.Fatalf("Apply first: %v", err)
	}

	// Second apply with fewer mappings : the old rules must be gone,
	// only the new one remains.
	if err := r.Apply([]NATMapping{
		{PublicIP: "203.0.113.99", PrivateIP: "10.0.0.99", VMName: "new"},
	}); err != nil {
		t.Fatalf("Apply second: %v", err)
	}

	c, err := nft.New(nft.AsLasting())
	if err != nil {
		t.Fatalf("nft.New: %v", err)
	}
	defer c.CloseLasting()

	tables, _ := c.ListTablesOfFamily(nft.TableFamilyIPv4)
	chains, _ := c.ListChainsOfTableFamily(nft.TableFamilyIPv4)
	var pre, post *nft.Chain
	var fipTable *nft.Table
	for _, tb := range tables {
		if tb.Name == natTableName {
			fipTable = tb
		}
	}
	for _, ch := range chains {
		if ch.Table == nil || ch.Table.Name != natTableName {
			continue
		}
		switch ch.Name {
		case "prerouting":
			pre = ch
		case "postrouting":
			post = ch
		}
	}
	if fipTable == nil || pre == nil || post == nil {
		t.Fatalf("post-reapply layout missing : table=%v pre=%v post=%v", fipTable, pre, post)
	}

	preRules, _ := c.GetRules(fipTable, pre)
	postRules, _ := c.GetRules(fipTable, post)
	if len(preRules) != 1 || len(postRules) != 1 {
		t.Errorf("expected 1 rule per chain after replace, got pre=%d post=%d", len(preRules), len(postRules))
	}
	if !anyRuleHas(preRules, "new") {
		t.Errorf("replaced state does not carry the new VM name : %v", dumpUserData(preRules))
	}
	if anyRuleHas(preRules, "old-1") || anyRuleHas(preRules, "old-2") || anyRuleHas(preRules, "old-3") {
		t.Errorf("stale rules survived the replace : %v", dumpUserData(preRules))
	}
}

// anyRuleHas returns true if any rule's UserData contains needle as
// a substring. Cheap and good enough — the reconciler encodes the
// vm name + kind via ruleComment which packs them as a comment TLV.
func anyRuleHas(rules []*nft.Rule, needle string) bool {
	nb := []byte(needle)
	for _, r := range rules {
		if containsBytes(r.UserData, nb) {
			return true
		}
	}
	return false
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func dumpUserData(rules []*nft.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, string(r.UserData))
	}
	return out
}
