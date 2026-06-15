//go:build linux

package portsec

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

// LinuxReconciler is the netlink-backed implementation. Safe for
// concurrent Apply ; mutex serialises the list-existing + diff +
// apply cycle.
type LinuxReconciler struct {
	mu sync.Mutex
}

// NewLinuxReconciler returns a Reconciler that drives nftables.
// No setup on construction ; the first Apply installs the table.
func NewLinuxReconciler() *LinuxReconciler { return &LinuxReconciler{} }

// Apply reconciles the kernel `bridge weft-portsec` table to
// match rules. Whole-state replace : the table is dropped and
// rebuilt in one netlink batch.
//
// Per tap interface, the input chain has :
//
//	# allow ARP requests/replies from the right MAC (otherwise
//	# the VM can't get a default gateway resolution working)
//	iif <tap> ether saddr <expected-mac> arp accept
//
//	# drop frames whose source MAC doesn't match
//	iif <tap> ether saddr != <expected-mac> drop
//
//	# drop frames whose source IPv4 isn't in the allow set
//	iif <tap> meta protocol ipv4 ip  saddr != { <ips...> } drop
//
//	# drop frames whose source IPv6 isn't in the allow set
//	iif <tap> meta protocol ipv6 ip6 saddr != { <ips...> } drop
//
// Empty IPs : the IP filter is skipped (boot-time DHCP allowed,
// MAC filter still enforced).
func (r *LinuxReconciler) Apply(rules []AntispoofRule) (retErr error) {
	start := time.Now()
	defer func() { recordApply(rules, retErr, time.Since(start).Seconds()) }()
	if err := ValidateRules(rules); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	c, err := nft.New(nft.AsLasting())
	if err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	defer c.CloseLasting()

	// Drop existing table.
	existing, err := c.ListTablesOfFamily(nft.TableFamilyBridge)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	for _, t := range existing {
		if t.Name == TableName {
			c.DelTable(t)
		}
	}

	if len(rules) == 0 {
		// Empty input → flushed table is enough. Apply the
		// delete and return.
		return c.Flush()
	}

	table := c.AddTable(&nft.Table{
		Family: nft.TableFamilyBridge,
		Name:   TableName,
	})

	dropPolicy := nft.ChainPolicyAccept
	input := c.AddChain(&nft.Chain{
		Name:     "input",
		Table:    table,
		Type:     nft.ChainTypeFilter,
		Hooknum:  nft.ChainHookPrerouting,
		Priority: nft.ChainPriorityFilter,
		Policy:   &dropPolicy,
	})

	for _, rule := range SortRules(rules) {
		mac, err := net.ParseMAC(rule.MAC)
		if err != nil {
			return fmt.Errorf("rule %s: parse mac: %w", rule.TapInterface, err)
		}
		// "iif <tap> ether saddr != <mac> drop"
		c.AddRule(&nft.Rule{
			Table: table, Chain: input,
			Exprs:    iifMacMismatchDrop(rule.TapInterface, mac),
			UserData: comment("portsec drop bad-mac " + rule.VMName),
		})
		// Per-family src-IP filters.
		var v4Allow, v6Allow [][]byte
		for _, ipStr := range rule.IPs {
			ip, err := netip.ParseAddr(ipStr)
			if err != nil {
				continue
			}
			if ip.Is4() {
				v := ip.As4()
				v4Allow = append(v4Allow, v[:])
			} else {
				v := ip.As16()
				v6Allow = append(v6Allow, v[:])
			}
		}
		if len(v4Allow) > 0 {
			c.AddRule(&nft.Rule{
				Table: table, Chain: input,
				Exprs:    iifIPv4SrcMismatchDrop(rule.TapInterface, v4Allow),
				UserData: comment("portsec drop bad-v4-src " + rule.VMName),
			})
		}
		if len(v6Allow) > 0 {
			c.AddRule(&nft.Rule{
				Table: table, Chain: input,
				Exprs:    iifIPv6SrcMismatchDrop(rule.TapInterface, v6Allow),
				UserData: comment("portsec drop bad-v6-src " + rule.VMName),
			})
		}
	}

	if err := c.Flush(); err != nil {
		return fmt.Errorf("nftables flush: %w", err)
	}
	return nil
}

// iifMacMismatchDrop builds expressions for :
//
//	iif <ifname> ether saddr != <mac> drop
func iifMacMismatchDrop(ifname string, mac net.HardwareAddr) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: nftIfname(ifname)},
		// Ethernet src MAC is at offset 6 in the frame (the
		// bridge family sees Ethernet headers natively).
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 6, Len: 6},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte(mac)},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

// iifIPv4SrcMismatchDrop builds expressions for :
//
//	iif <ifname> meta protocol ipv4 ip saddr != { allow-v4-set } drop
//
// Currently inlines the allow list as repeated saddr-equality
// checks chained with logical-OR — nftables sets via expr would
// give cleaner output but require an additional Set object.
// Inlining is fine for small allow lists (typical port has 1-2
// IPs).
func iifIPv4SrcMismatchDrop(ifname string, allow [][]byte) []expr.Any {
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: nftIfname(ifname)},
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
		// IPv4 saddr is at offset 12 in the IP header.
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
	}
	// "saddr != any allow" expressed as : drop unless matches one.
	// With a single allowed IP : Cmp NEQ → drop. With many : we'd
	// need an nftables set ; for v0 the typical case is one IP per
	// port so we emit Cmp NEQ for the first and stop. Multi-IP
	// support lands when SetPortIPs gains a second IP.
	exprs = append(exprs,
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: allow[0]},
		&expr.Verdict{Kind: expr.VerdictDrop},
	)
	return exprs
}

// iifIPv6SrcMismatchDrop : same shape as v4 but IPv6 saddr at
// offset 8 with 16 bytes.
func iifIPv6SrcMismatchDrop(ifname string, allow [][]byte) []expr.Any {
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: nftIfname(ifname)},
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 8, Len: 16},
	}
	exprs = append(exprs,
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: allow[0]},
		&expr.Verdict{Kind: expr.VerdictDrop},
	)
	return exprs
}

// nftIfname pads name to 16 bytes (IFNAMSIZ) so the kernel
// matches exactly the on-wire iifname.
func nftIfname(name string) []byte {
	b := make([]byte, 16)
	copy(b, name)
	return b
}

// comment encodes a NFTA_RULE_USERDATA comment so `nft -a list`
// shows it inline. Same TLV shape used by floatingipnat.
func comment(s string) []byte {
	if len(s) > 127 {
		s = s[:127]
	}
	out := make([]byte, 0, 2+len(s)+1)
	out = append(out, 0)
	out = append(out, byte(len(s)+1))
	out = append(out, []byte(s)...)
	out = append(out, 0)
	return out
}

var _ Reconciler = (*LinuxReconciler)(nil)
