//go:build linux

package portqos

import (
	"fmt"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
)

// LinuxReconciler is the netlink-backed tc/htb implementation.
type LinuxReconciler struct {
	mu sync.Mutex
}

// NewLinuxReconciler returns a Reconciler. No setup on construct.
func NewLinuxReconciler() *LinuxReconciler { return &LinuxReconciler{} }

// Apply reconciles per-tap shaping. For each spec :
//
//	1. Lookup the tap link by name (skip with a warning if absent —
//	   driver may not have created it yet at the time the event
//	   landed ; next reconcile picks it up).
//	2. Install HTB root qdisc + class with rate = EgressMbps.
//	   This caps packets leaving the host TOWARD the VM (the
//	   tap's egress queue from the host's POV).
//	3. For ingress (packets leaving the VM toward the host) :
//	   redirect to an ifb device "<tap>-ifb" via tc filter, then
//	   shape the ifb's egress with the IngressMbps rate.
//	   ifb is the standard linux trick for "ingress shaping" since
//	   the kernel doesn't allow direct shaping on ingress qdiscs.
//
// Zero rates skip the corresponding step (so a spec with only
// EgressMbps set is fine).
//
// Whole-state replace : the reconciler also walks every existing
// "<tap>-ifb" the kernel has and removes any not in the new set.
// (HTB qdisc on the tap is replaced in-place by netlink's
// QdiscReplace.)
func (r *LinuxReconciler) Apply(specs []PortQoS) (retErr error) {
	start := time.Now()
	defer func() { recordApply(specs, retErr, time.Since(start).Seconds()) }()
	if err := ValidateSpecs(specs); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Build the target set for cleanup.
	managedIFB := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		managedIFB[ifbName(s.TapInterface)] = struct{}{}
	}

	// Cleanup orphan ifb devices the reconciler used to own.
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	for _, l := range links {
		name := l.Attrs().Name
		if l.Type() != "ifb" {
			continue
		}
		if _, keep := managedIFB[name]; keep {
			continue
		}
		if !looksLikeWeftIFB(name) {
			continue
		}
		_ = netlink.LinkDel(l) // best-effort, kernel handles in-use refusal
	}

	for _, s := range specs {
		if err := r.applyOne(s); err != nil {
			return fmt.Errorf("tap %s: %w", s.TapInterface, err)
		}
	}
	return nil
}

func (r *LinuxReconciler) applyOne(s PortQoS) error {
	tap, err := netlink.LinkByName(s.TapInterface)
	if err != nil {
		// Driver hasn't created the tap yet — log + skip ; the
		// next reconcile retries.
		return nil
	}

	// Egress (host → VM) : root HTB on the tap.
	if s.EgressMbps > 0 {
		if err := installEgressHTB(tap, s.EgressMbps); err != nil {
			return fmt.Errorf("egress htb: %w", err)
		}
	} else {
		// No cap requested → drop any existing HTB on the tap.
		_ = clearRootHTB(tap)
	}

	// Ingress (VM → host) : redirect tap ingress to ifb, shape
	// ifb egress with IngressMbps.
	if s.IngressMbps > 0 {
		if err := installIngressHTB(tap, s.TapInterface, s.IngressMbps); err != nil {
			return fmt.Errorf("ingress htb: %w", err)
		}
	} else {
		// No cap → drop ifb if present.
		if ifb, err := netlink.LinkByName(ifbName(s.TapInterface)); err == nil {
			_ = netlink.LinkDel(ifb)
		}
	}
	return nil
}

// installEgressHTB sets up HTB root qdisc + class on the tap.
//
//	tc qdisc replace dev <tap> root handle 1: htb default 10
//	tc class replace dev <tap> parent 1: classid 1:10 htb \
//	    rate <mbps>mbit ceil <mbps>mbit burst <mbps * 1Mbit/s>
func installEgressHTB(tap netlink.Link, mbps int) error {
	root := &netlink.Htb{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: tap.Attrs().Index,
			Handle:    netlink.MakeHandle(RootHandleMajor, 0),
			Parent:    netlink.HANDLE_ROOT,
		},
		Defcls: ClassHandleMinor,
		Rate2Quantum: 10,
		Version:      3,
	}
	if err := netlink.QdiscReplace(root); err != nil {
		return fmt.Errorf("qdisc replace: %w", err)
	}
	rateBps := uint64(mbps) * 1_000_000 / 8 // Mbps → bytes/s
	cls := &netlink.HtbClass{
		ClassAttrs: netlink.ClassAttrs{
			LinkIndex: tap.Attrs().Index,
			Handle:    netlink.MakeHandle(RootHandleMajor, ClassHandleMinor),
			Parent:    netlink.MakeHandle(RootHandleMajor, 0),
		},
		Rate:    rateBps,
		Ceil:    rateBps,
		Buffer:  uint32(rateBps / 800), // ~10ms burst
		Cbuffer: uint32(rateBps / 800),
	}
	if err := netlink.ClassReplace(cls); err != nil {
		return fmt.Errorf("class replace: %w", err)
	}
	return nil
}

// clearRootHTB removes the root HTB qdisc (back to pfifo_fast).
func clearRootHTB(tap netlink.Link) error {
	root := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: tap.Attrs().Index,
			Handle:    netlink.MakeHandle(RootHandleMajor, 0),
			Parent:    netlink.HANDLE_ROOT,
		},
	}
	return netlink.QdiscDel(root)
}

// installIngressHTB sets up the ifb mirror + ingress shaping.
//
//	# 1. Create ifb device "<tap>-ifb" if missing, bring it up.
//	# 2. Add ingress qdisc on tap.
//	# 3. Add filter on tap ingress : action mirred egress redirect dev <ifb>.
//	# 4. Add HTB on <ifb> with the rate cap (same shape as egress).
func installIngressHTB(tap netlink.Link, tapName string, mbps int) error {
	ifb, err := netlink.LinkByName(ifbName(tapName))
	if err != nil {
		// Create it.
		ifbLink := &netlink.GenericLink{
			LinkAttrs: netlink.LinkAttrs{Name: ifbName(tapName)},
			LinkType:  "ifb",
		}
		if err := netlink.LinkAdd(ifbLink); err != nil {
			return fmt.Errorf("create ifb: %w", err)
		}
		ifb, _ = netlink.LinkByName(ifbName(tapName))
	}
	if err := netlink.LinkSetUp(ifb); err != nil {
		return fmt.Errorf("ifb up: %w", err)
	}

	// Ingress qdisc on tap (handle ffff:).
	ingress := &netlink.Ingress{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: tap.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_INGRESS,
		},
	}
	_ = netlink.QdiscAdd(ingress) // idempotent — exists is fine

	// Filter : everything → mirred egress redirect to ifb.
	filter := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: tap.Attrs().Index,
			Parent:    netlink.MakeHandle(0xffff, 0),
			Priority:  1,
			Protocol:  0x0003, // ETH_P_ALL
		},
		Actions: []netlink.Action{
			&netlink.MirredAction{
				ActionAttrs:  netlink.ActionAttrs{Action: netlink.TC_ACT_STOLEN},
				MirredAction: netlink.TCA_EGRESS_REDIR,
				Ifindex:      ifb.Attrs().Index,
			},
		},
	}
	_ = netlink.FilterDel(filter)
	if err := netlink.FilterAdd(filter); err != nil {
		return fmt.Errorf("redirect filter: %w", err)
	}

	// HTB on ifb (shapes the redirected traffic).
	rateBps := uint64(mbps) * 1_000_000 / 8
	ifbRoot := &netlink.Htb{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: ifb.Attrs().Index,
			Handle:    netlink.MakeHandle(RootHandleMajor, 0),
			Parent:    netlink.HANDLE_ROOT,
		},
		Defcls:       ClassHandleMinor,
		Rate2Quantum: 10,
		Version:      3,
	}
	if err := netlink.QdiscReplace(ifbRoot); err != nil {
		return fmt.Errorf("ifb qdisc: %w", err)
	}
	cls := &netlink.HtbClass{
		ClassAttrs: netlink.ClassAttrs{
			LinkIndex: ifb.Attrs().Index,
			Handle:    netlink.MakeHandle(RootHandleMajor, ClassHandleMinor),
			Parent:    netlink.MakeHandle(RootHandleMajor, 0),
		},
		Rate:    rateBps,
		Ceil:    rateBps,
		Buffer:  uint32(rateBps / 800),
		Cbuffer: uint32(rateBps / 800),
	}
	if err := netlink.ClassReplace(cls); err != nil {
		return fmt.Errorf("ifb class: %w", err)
	}
	return nil
}

// ifbName returns the ifb device name backing tap. Capped at
// IFNAMSIZ-1 = 15 chars by the kernel ; suffix "-ifb" eats 4
// chars so tapName ≤ 11. Callers should keep tap names short.
func ifbName(tapName string) string {
	const suffix = "-ifb"
	max := 15 - len(suffix)
	if len(tapName) > max {
		tapName = tapName[:max]
	}
	return tapName + suffix
}

// looksLikeWeftIFB matches the "-ifb" suffix convention. Used by
// the cleanup pass so we don't touch ifb devices owned by other
// tools.
func looksLikeWeftIFB(name string) bool {
	return len(name) > 4 && name[len(name)-4:] == "-ifb"
}

var _ Reconciler = (*LinuxReconciler)(nil)
