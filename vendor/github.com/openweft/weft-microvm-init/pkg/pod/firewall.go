package pod

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
)

// Firewall is the per-VM desired stateful filter ruleset. weft publishes
// this whole-state on the event bus (subject "weft.firewall.<vm-uuid>")
// whenever the effective rules for the VM change (Security-Group rules
// edited, group attached/detached, network defaults changed). The agent
// inside the microVM atomically reconciles its nftables table against
// the new state — replace-set, idempotent, missed-message-self-heals
// — same model pkg/mesh / pkg/mounts use for their concerns.
//
// The set is pre-resolved by the publisher: every Security-Group rule
// that references another group by UUID is expanded into one or more
// FirewallRule entries with concrete RemoteCIDR values. The guest never
// sees group references and never has to query the control plane.
type Firewall struct {
	Rules []FirewallRule `json:"rules"`
}

// FirewallRule mirrors weft-proto's SecurityRule but with remote_group_uuid
// already dereferenced to a CIDR (or empty = any). Stateful: the reconciler
// installs the rule on the matching hook (ingress/egress) with conntrack
// established/related already accepted at the top of the chain.
type FirewallRule struct {
	// Direction is "ingress" (traffic into the VM) or "egress" (traffic
	// out of the VM). Empty is rejected by Validate.
	Direction string `json:"direction"`
	// Protocol is "tcp", "udp", "icmp" (any code/type), or "" meaning any
	// L4 protocol. PortMin/PortMax are ignored when Protocol is "" or
	// "icmp".
	Protocol string `json:"protocol,omitempty"`
	// PortMin / PortMax describe an inclusive destination port range
	// (for ingress) or source port range (for egress). 0 = unset; if
	// set, PortMin must be ≤ PortMax. A single port is encoded as
	// PortMin == PortMax.
	PortMin uint16 `json:"port_min,omitempty"`
	PortMax uint16 `json:"port_max,omitempty"`
	// RemoteCIDR is the peer subnet matched on the rule. Empty means
	// any. Validated as a parseable CIDR by Validate.
	RemoteCIDR string `json:"remote_cidr,omitempty"`
}

// LoadFirewall reads a standalone firewall ruleset from path. Returns
// (nil, nil) if the file does not exist — boot without a pre-staged
// ruleset is legal; the agent will receive the first state via NATS.
func LoadFirewall(path string) (*Firewall, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var fw Firewall
	if err := json.Unmarshal(b, &fw); err != nil {
		return nil, fmt.Errorf("firewall config %s: %w", path, err)
	}
	if err := fw.Validate(); err != nil {
		return nil, fmt.Errorf("firewall config %s: %w", path, err)
	}
	return &fw, nil
}

// Validate checks every rule has a recognised Direction and Protocol,
// a coherent port range, and a parseable RemoteCIDR. The empty ruleset
// is valid and means "default policy only" — typically default-deny
// ingress, default-allow egress at the chain level.
func (f *Firewall) Validate() error {
	for i, r := range f.Rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("rule[%d]: %w", i, err)
		}
	}
	return nil
}

// Validate checks a single rule. Pulled out so the host-side publisher
// can validate one rule at a time when expanding remote_group_uuid.
func (r FirewallRule) Validate() error {
	switch r.Direction {
	case "ingress", "egress":
	default:
		return fmt.Errorf("direction %q: must be ingress or egress", r.Direction)
	}
	switch r.Protocol {
	case "", "tcp", "udp", "icmp":
	default:
		return fmt.Errorf("protocol %q: must be tcp, udp, icmp or empty", r.Protocol)
	}
	if r.PortMin != 0 || r.PortMax != 0 {
		if r.Protocol != "tcp" && r.Protocol != "udp" {
			return fmt.Errorf("port range requires tcp or udp, got %q", r.Protocol)
		}
		if r.PortMin > r.PortMax {
			return fmt.Errorf("port_min %d > port_max %d", r.PortMin, r.PortMax)
		}
	}
	if r.RemoteCIDR != "" {
		if _, err := netip.ParsePrefix(r.RemoteCIDR); err != nil {
			return fmt.Errorf("remote_cidr %q: %w", r.RemoteCIDR, err)
		}
	}
	return nil
}
