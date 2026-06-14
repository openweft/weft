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
	// hooks). Both tables share the same name "weft-fip-nat"
	// — distinct families = distinct kernel objects, no conflict.
	c, err := nft.New(nft.AsLasting())
	if err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	defer c.CloseLasting()

	// Drop the existing tables (both families) if present, then
	// rebuild from scratch in the same batch.
	for _, fam := range []nft.TableFamily{nft.TableFamilyIPv4, nft.TableFamilyIPv6} {
		existing, err := c.ListTablesOfFamily(fam)
		if err != nil {
			return fmt.Errorf("list tables (fam %d): %w", fam, err)
		}
		for _, t := range existing {
			if t.Name == natTableName {
				c.DelTable(t)
			}
		}
	}

	table4 := c.AddTable(&nft.Table{Family: nft.TableFamilyIPv4, Name: natTableName})
	table6 := c.AddTable(&nft.Table{Family: nft.TableFamilyIPv6, Name: natTableName})

	prerouting4 := c.AddChain(&nft.Chain{
		Name: "prerouting", Table: table4,
		Type: nft.ChainTypeNAT, Hooknum: nft.ChainHookPrerouting,
		Priority: nft.ChainPriorityNATDest,
	})
	postrouting4 := c.AddChain(&nft.Chain{
		Name: "postrouting", Table: table4,
		Type: nft.ChainTypeNAT, Hooknum: nft.ChainHookPostrouting,
		Priority: nft.ChainPriorityNATSource,
	})
	prerouting6 := c.AddChain(&nft.Chain{
		Name: "prerouting", Table: table6,
		Type: nft.ChainTypeNAT, Hooknum: nft.ChainHookPrerouting,
		Priority: nft.ChainPriorityNATDest,
	})
	postrouting6 := c.AddChain(&nft.Chain{
		Name: "postrouting", Table: table6,
		Type: nft.ChainTypeNAT, Hooknum: nft.ChainHookPostrouting,
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
		// Refuse mixed-family mappings — DNAT must rewrite within
		// the same family. The control plane should never produce
		// one, but defend in depth.
		if pub.Is4() != prv.Is4() {
			return fmt.Errorf("mapping %s/%s : address family mismatch", m.PublicIP, m.PrivateIP)
		}
		isV4 := pub.Is4()
		var preroutingChain, postroutingChain *nft.Chain
		var ruleTable *nft.Table
		var pubBytes, prvBytes []byte
		if isV4 {
			pub4 := pub.As4()
			prv4 := prv.As4()
			pubBytes, prvBytes = pub4[:], prv4[:]
			preroutingChain, postroutingChain = prerouting4, postrouting4
			ruleTable = table4
		} else {
			pub16 := pub.As16()
			prv16 := prv.As16()
			pubBytes, prvBytes = pub16[:], prv16[:]
			preroutingChain, postroutingChain = prerouting6, postrouting6
			ruleTable = table6
		}
		// Optional rate limit : when RateLimitPPS > 0, insert a
		// drop-on-over-rate rule BEFORE the DNAT so a DDoS spray
		// against the FIP is silently dropped before the
		// connection state is even instantiated. The DNAT rule
		// below only fires for under-rate packets.
		if m.RateLimitPPS > 0 {
			c.AddRule(&nft.Rule{
				Table:    ruleTable,
				Chain:    preroutingChain,
				Exprs:    rateLimitDropExprs2(pubBytes, isV4, uint64(m.RateLimitPPS)),
				UserData: ruleComment(m.VMName, "ratelimit"),
			})
		}
		c.AddRule(&nft.Rule{
			Table:    ruleTable,
			Chain:    preroutingChain,
			Exprs:    dnatExprs2(pubBytes, prvBytes, isV4),
			UserData: ruleComment(m.VMName, "dnat"),
		})
		c.AddRule(&nft.Rule{
			Table:    ruleTable,
			Chain:    postroutingChain,
			Exprs:    snatExprs2(prvBytes, pubBytes, isV4),
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

// dnatExprs2 generalises dnatExprs across v4 + v6. The shape is :
//
//	meta nfproto ipv{4,6}
//	payload @ip header (saddr/daddr) of right length
//	cmp eq <publicIP>
//	immediate <privateIP>
//	nat dnat
//
// IPv4 daddr is at offset 16, length 4 ; IPv6 daddr is at offset
// 24, length 16. The NAT expression's Family must match.
func dnatExprs2(publicIP, privateIP []byte, isV4 bool) []expr.Any {
	if isV4 {
		return dnatExprs(publicIP, privateIP)
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 24, Len: 16},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: publicIP},
		&expr.Immediate{Register: 1, Data: privateIP},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV6, RegAddrMin: 1},
	}
}

// snatExprs2 : same idea for SNAT. IPv6 saddr is at offset 8.
func snatExprs2(privateIP, publicIP []byte, isV4 bool) []expr.Any {
	if isV4 {
		return snatExprs(privateIP, publicIP)
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 8, Len: 16},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: privateIP},
		&expr.Immediate{Register: 1, Data: publicIP},
		&expr.NAT{Type: expr.NATTypeSourceNAT, Family: unix.NFPROTO_IPV6, RegAddrMin: 1},
	}
}

// rateLimitDropExprs2 : v4 reuses the original ; v6 matches IPv6
// daddr at offset 24 with the same limit clause.
func rateLimitDropExprs2(publicIP []byte, isV4 bool, pps uint64) []expr.Any {
	if isV4 {
		return rateLimitDropExprs(publicIP, pps)
	}
	burst := uint32(pps * 2)
	if burst < 5 {
		burst = 5
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 24, Len: 16},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: publicIP},
		&expr.Limit{Type: expr.LimitTypePkts, Rate: pps, Unit: expr.LimitTimeSecond, Burst: burst, Over: true},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

// dnatExprs builds the expression list for
// `ip daddr <publicIP> dnat to <privateIP>`. IPv4-only.
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
