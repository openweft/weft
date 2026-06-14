//go:build linux

package floatingipnat

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	nft "github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// natTableName is the table the reconciler owns end-to-end. Other
// tables on the host (e.g. operator-installed NAT rules, fail2ban,
// docker) are untouched.
const natTableName = "weft-fip-nat"

// LinuxReconciler is the netlink-backed Apply path. Safe for
// concurrent use ; Apply serialises on its own mutex so two
// callers don't race the flush-then-rebuild cycle.
type LinuxReconciler struct {
	mu sync.Mutex
}

// NewLinuxReconciler returns a Reconciler that drives nftables
// via netlink. No setup work happens here ; the first Apply
// installs the table.
func NewLinuxReconciler() *LinuxReconciler { return &LinuxReconciler{} }

// Apply replaces the host's "weft-fip-nat" table whole. Atomic
// at the netlink batch level — an outside observer never sees a
// half-applied policy.
//
// Records weft_fip_nat_apply_total / _duration / _rules_installed
// via the package-level metrics surface on every call ; the deferred
// recordApply observes the result regardless of which return path
// fires (ValidateMappings reject, netlink failure, success).
func (r *LinuxReconciler) Apply(mappings []NATMapping) (retErr error) {
	start := time.Now()
	defer func() {
		recordApply(mappings, retErr, time.Since(start).Seconds())
	}()
	if err := ValidateMappings(mappings); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Split by family ; v4 and v6 NAT live in separate tables
	// (nftables doesn't allow inet-family NAT in the dst/srcnat
	// hooks). Today we only build the IPv4 table — v6 is left
	// for a follow-up when the platform actually allocates v6
	// floating IPs (the proto admits it but no edge network is
	// configured for v6 yet in the cluster.hcl). When that lands
	// this function gets a second pass over the v6 mappings.
	c, err := nft.New(nft.AsLasting())
	if err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	defer c.CloseLasting()

	// Drop the existing table if present, then rebuild from
	// scratch in the same batch.
	existing, err := c.ListTablesOfFamily(nft.TableFamilyIPv4)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	for _, t := range existing {
		if t.Name == natTableName {
			c.DelTable(t)
		}
	}

	table := c.AddTable(&nft.Table{
		Family: nft.TableFamilyIPv4,
		Name:   natTableName,
	})

	// DNAT chain : type nat hook prerouting priority dstnat.
	// Rules rewrite ip daddr <publicIP> → <privateIP>.
	prerouting := c.AddChain(&nft.Chain{
		Name:     "prerouting",
		Table:    table,
		Type:     nft.ChainTypeNAT,
		Hooknum:  nft.ChainHookPrerouting,
		Priority: nft.ChainPriorityNATDest,
	})
	// SNAT chain : type nat hook postrouting priority srcnat.
	// Rules rewrite ip saddr <privateIP> → <publicIP>.
	postrouting := c.AddChain(&nft.Chain{
		Name:     "postrouting",
		Table:    table,
		Type:     nft.ChainTypeNAT,
		Hooknum:  nft.ChainHookPostrouting,
		Priority: nft.ChainPriorityNATSource,
	})

	for _, m := range mappings {
		pub, err := netip.ParseAddr(m.PublicIP)
		if err != nil {
			return fmt.Errorf("public_ip %q: %w", m.PublicIP, err)
		}
		prv, err := netip.ParseAddr(m.PrivateIP)
		if err != nil {
			return fmt.Errorf("private_ip %q: %w", m.PrivateIP, err)
		}
		// V6 handled in a follow-up — see the comment at the top
		// of Apply.
		if !pub.Is4() || !prv.Is4() {
			continue
		}
		pub4 := pub.As4()
		prv4 := prv.As4()
		// Optional rate limit : when RateLimitPPS > 0, insert a
		// drop-on-over-rate rule BEFORE the DNAT so a DDoS spray
		// against the FIP is silently dropped before the
		// connection state is even instantiated. The DNAT rule
		// below only fires for under-rate packets.
		if m.RateLimitPPS > 0 {
			c.AddRule(&nft.Rule{
				Table:    table,
				Chain:    prerouting,
				Exprs:    rateLimitDropExprs(pub4[:], uint64(m.RateLimitPPS)),
				UserData: ruleComment(m.VMName, "ratelimit"),
			})
		}
		c.AddRule(&nft.Rule{
			Table:    table,
			Chain:    prerouting,
			Exprs:    dnatExprs(pub4[:], prv4[:]),
			UserData: ruleComment(m.VMName, "dnat"),
		})
		c.AddRule(&nft.Rule{
			Table:    table,
			Chain:    postrouting,
			Exprs:    snatExprs(prv4[:], pub4[:]),
			UserData: ruleComment(m.VMName, "snat"),
		})
	}

	if err := c.Flush(); err != nil {
		return fmt.Errorf("nftables flush: %w", err)
	}
	return nil
}

// rateLimitDropExprs builds the expression list for :
//
//	ip daddr <publicIP> limit rate over <pps>/second burst <2*pps> drop
//
// Matches IPv4 destination = publicIP AND packet rate is over the
// configured pps, then drops. The DNAT rule that follows in the
// same chain only fires for under-rate traffic.
//
// Burst = 2*rate (one-second leeway) — generous enough not to drop
// TCP slow-start bursts on legitimate flows.
func rateLimitDropExprs(publicV4 []byte, pps uint64) []expr.Any {
	burst := uint32(pps * 2)
	if burst < 5 {
		burst = 5 // minimum that lets a 3-way handshake through
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader,
			Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: publicV4},
		&expr.Limit{
			Type:  expr.LimitTypePkts,
			Rate:  pps,
			Unit:  expr.LimitTimeSecond,
			Burst: burst,
			Over:  true, // match when over the rate → drop
		},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

// dnatExprs builds the expression list for
// `ip daddr <publicIP> dnat to <privateIP>`. IPv4-only for now.
func dnatExprs(publicV4, privateV4 []byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader,
			Offset: 16, Len: 4}, // ip daddr
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: publicV4},
		&expr.Immediate{Register: 1, Data: privateV4},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
	}
}

// snatExprs builds the expression list for
// `ip saddr <privateIP> snat to <publicIP>`.
func snatExprs(privateV4, publicV4 []byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader,
			Offset: 12, Len: 4}, // ip saddr
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: privateV4},
		&expr.Immediate{Register: 1, Data: publicV4},
		&expr.NAT{Type: expr.NATTypeSourceNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
	}
}

// ruleComment encodes a short comment in the NFTA_RULE_USERDATA
// type-length-value envelope nftables uses. Best-effort : the
// data plane works without it, but `nft -a list ruleset` shows
// it on display so an operator immediately sees which VM a rule
// belongs to.
func ruleComment(vmName, kind string) []byte {
	comment := fmt.Sprintf("weft-fip %s %s", kind, vmName)
	if len(comment) > 127 {
		comment = comment[:127]
	}
	// NFTNL_UDATA_RULE_COMMENT = 0 ; format is type(1) + len(1) + data.
	out := make([]byte, 0, 2+len(comment)+1)
	out = append(out, 0)                          // type = comment
	out = append(out, byte(len(comment)+1))       // len incl. NUL
	out = append(out, []byte(comment)...)
	out = append(out, 0)                          // NUL terminator
	return out
}

// Compile-time check : the linux impl satisfies the public
// interface. Helps catch a refactor that breaks the contract.
var _ Reconciler = (*LinuxReconciler)(nil)

// Unused on linux — kept to silence the unused-import warning
// in builds that strip down to the bare reconciler.
var _ = net.ParseIP
