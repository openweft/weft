// Package portqos is the host-side bandwidth-limit reconciler.
// Applies per-tap-interface ingress + egress rate caps using
// tc/htb (Hierarchical Token Bucket) via netlink.
//
// "Egress" from the host's perspective : packets leaving the host
// through the tap into the VM (i.e. inbound for the VM). HTB on
// the root qdisc throttles this.
//
// "Ingress" from the host's perspective : packets leaving the VM
// through the tap onto the host (outbound for the VM). HTB on the
// ifb (Intermediate Functional Block) device that the tap mirrors
// to. The mirror is set up via tc filter ; we hide that detail
// behind the same Apply call.
//
// One rate cap per direction. Bursts are sized at the rate (1 s
// of traffic) — generous enough to avoid choking TCP slow-start,
// strict enough to enforce the cap on sustained traffic.
//
// Architecture :
//
//	weft (control plane) ─ port.{created,updated,deleted}
//	                                ▼
//	portqos.Watcher (next slice : driver surfaces tap names)
//	                                ▼
//	LinuxReconciler.Apply ─ tc qdisc/class/filter via netlink
//	                                ▼
//	kernel HTB shapes the tap traffic to the configured rates
package portqos

import (
	"fmt"
)

// PortQoS is one tap interface's rate caps. Zero values disable
// shaping for that direction (no cap), so the typical operator
// flow is "set just the directions you care about".
type PortQoS struct {
	// TapInterface is the host-side kernel interface name backing
	// the VM's NIC. Required.
	TapInterface string
	// IngressMbps is the cap on traffic FROM the VM toward the
	// rest of the network (host's POV : packets arriving on the
	// tap). 0 = no cap. Range 1-100000 Mbps.
	IngressMbps int
	// EgressMbps is the cap on traffic TO the VM (host's POV :
	// packets leaving the host through the tap). 0 = no cap.
	// Range 1-100000 Mbps.
	EgressMbps int
	// VMName is informational, used in log lines.
	VMName string
}

// Validate checks one QoS spec. Empty TapInterface rejected ;
// rates must be in [0, 100_000].
func (q PortQoS) Validate() error {
	if q.TapInterface == "" {
		return fmt.Errorf("empty tap_interface")
	}
	if len(q.TapInterface) > 15 {
		return fmt.Errorf("tap_interface %q too long (>15)", q.TapInterface)
	}
	if q.IngressMbps < 0 || q.IngressMbps > 100_000 {
		return fmt.Errorf("ingress_mbps out of range [0, 100000]: %d", q.IngressMbps)
	}
	if q.EgressMbps < 0 || q.EgressMbps > 100_000 {
		return fmt.Errorf("egress_mbps out of range [0, 100000]: %d", q.EgressMbps)
	}
	return nil
}

// Reconciler is the platform abstraction. Apply is whole-state :
// the kernel tc state for the listed taps is reconciled to match
// the input ; previously-managed taps NOT in the input have their
// shaping removed.
type Reconciler interface {
	Apply(specs []PortQoS) error
}

// ValidateSpecs checks every spec + rejects duplicate taps.
func ValidateSpecs(specs []PortQoS) error {
	seen := make(map[string]struct{})
	for i, s := range specs {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("spec[%d]: %w", i, err)
		}
		if _, dup := seen[s.TapInterface]; dup {
			return fmt.Errorf("spec[%d]: tap %q appears twice", i, s.TapInterface)
		}
		seen[s.TapInterface] = struct{}{}
	}
	return nil
}

// HTBHandle is the major/minor handle the reconciler uses for the
// root qdisc + classes. Exported so an operator running
// `tc qdisc show dev <tap>` can recognise weft-owned shaping.
const (
	RootHandleMajor  = 0x1
	ClassHandleMinor = 0x10
)
