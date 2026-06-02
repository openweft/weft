package firewallpub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

// Scope is the wider read-only adapter view the publisher needs.
// Snapshot covers the resolver's lookups ; ListAllPorts powers
// impact analysis (which VMs need a refresh given a SG / network /
// port event). Kept as a separate interface so the resolver tests
// don't have to drag in the wider surface.
type Scope interface {
	Snapshot
	ListAllPorts() []weft.Port
}

// PublishFunc delivers one VM's freshly-resolved firewall to the
// transport (NATS in production, a channel/slice in tests). Returning
// an error lets the caller observe transport failures ; the Publisher
// logs them and moves on (a missed publish self-heals on the next
// event or the next Resync).
type PublishFunc func(vmUUID string, fw pod.Firewall) error

// Publisher is the event-driven bridge that turns Security-Group /
// Port / Network mutations into per-VM firewall publishes.
//
// One Publisher per weft process. Run blocks on an event channel
// (typically EventBus.Subscribe with KindPrefixes covering "vm.",
// "port.", "network.", "security_group.") and re-publishes every
// impacted VM's effective ruleset whenever a relevant event arrives.
// ResyncAll publishes the current state for a caller-supplied set
// of VMs ; the daemon should call it once at startup so a
// freshly-restarted weft pushes ground truth.
type Publisher struct {
	scope   Scope
	publish PublishFunc
	logger  *log.Logger
}

// New builds a Publisher. nil logger defaults to log.Default.
func New(scope Scope, publish PublishFunc, logger *log.Logger) *Publisher {
	if logger == nil {
		logger = log.Default()
	}
	return &Publisher{scope: scope, publish: publish, logger: logger}
}

// Run consumes events until ctx is cancelled or the channel closes.
// Each relevant event causes an effective-firewall recomputation +
// publish for every impacted VM. Irrelevant events are silently
// dropped.
func (p *Publisher) Run(ctx context.Context, events <-chan weft.PlatformEvent) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			vms := p.ImpactedVMs(ev)
			for _, vm := range vms {
				p.publishOne(vm)
			}
		}
	}
}

// ResyncAll computes and publishes the current effective firewall
// for every UUID in vmUUIDs. Use at startup, after a flag flip, or
// to recover from a deliberate flush. No-op on the empty list.
func (p *Publisher) ResyncAll(vmUUIDs []string) {
	for _, vm := range vmUUIDs {
		p.publishOne(vm)
	}
}

// publishOne resolves vmUUID's effective firewall and publishes it.
// Errors are logged + swallowed (transport hiccup, malformed snapshot).
// We re-validate before publishing so a buggy resolver path can never
// push a malformed payload past the network — the agent's
// HandleMessage rejects invalid updates, but rejecting at the publish
// site keeps the bus clean.
func (p *Publisher) publishOne(vmUUID string) {
	fw := EffectiveFirewall(p.scope, vmUUID)
	if err := fw.Validate(); err != nil {
		p.logger.Printf("firewallpub: vm %s: malformed effective firewall: %v", vmUUID, err)
		return
	}
	if err := p.publish(vmUUID, fw); err != nil {
		p.logger.Printf("firewallpub: vm %s: publish: %v", vmUUID, err)
		return
	}
	p.logger.Printf("firewallpub: vm %s: published %d rule(s)", vmUUID, len(fw.Rules))
}

// ImpactedVMs returns the distinct VM UUIDs that need a firewall
// refresh given ev. Pure (no IO, no publish), so it's the testable
// crux of the publisher.
//
// Coverage :
//   - security_group.{rules_updated,deleted,created}, plus
//     security_group.renamed (no behavioural change, but harmless to
//     republish — and we'd rather over-publish than miss one) :
//     every port that references the SG by UUID OR inherits it via
//     a network's defaults. The Subject field carries the SG UUID.
//   - port.{created,security_groups_updated,deleted} : the port's
//     VM (carried in Meta["vm_uuid"]).
//   - network.default_security_groups_updated : every VM with a
//     port on that network whose port carries no explicit SG list
//     (so it actually inherits the defaults). Subject is the
//     network UUID.
//   - vm.created : publish an initial empty/initial state for the
//     new VM so the agent has something to converge to (the agent
//     may have started before the first port was attached).
//     Subject is the VM UUID.
//
// Anything else returns nil ; the publisher silently skips it.
func (p *Publisher) ImpactedVMs(ev weft.PlatformEvent) []string {
	switch ev.Kind {
	case "security_group.rules_updated",
		"security_group.deleted",
		"security_group.created",
		"security_group.renamed":
		return p.vmsForSG(ev.Subject)

	case "port.created", "port.security_groups_updated", "port.deleted":
		if vmUUID := ev.Meta["vm_uuid"]; vmUUID != "" {
			return []string{vmUUID}
		}
		return nil

	case "network.default_security_groups_updated":
		return p.vmsInheritingFromNetwork(ev.Subject)

	case "vm.created":
		if ev.Subject != "" {
			return []string{ev.Subject}
		}
		return nil
	}
	return nil
}

// vmsForSG returns the distinct VM UUIDs whose ports reference
// sgUUID — either directly (Port.SecurityGroups includes it) or
// transitively (Port.SecurityGroups is empty AND the port's
// network's DefaultSecurityGroups includes it).
func (p *Publisher) vmsForSG(sgUUID string) []string {
	if sgUUID == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, port := range p.scope.ListAllPorts() {
		if !portReferencesSG(p.scope, port, sgUUID) {
			continue
		}
		if _, dup := seen[port.VMUUID]; dup {
			continue
		}
		seen[port.VMUUID] = struct{}{}
		out = append(out, port.VMUUID)
	}
	return out
}

// vmsInheritingFromNetwork returns VM UUIDs whose ports on
// networkUUID carry no explicit SG list — they're the ones affected
// by a change to that network's defaults. Ports with their own SG
// override don't see the change.
func (p *Publisher) vmsInheritingFromNetwork(networkUUID string) []string {
	if networkUUID == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, port := range p.scope.ListPortsForNetwork(networkUUID) {
		if len(port.SecurityGroups) != 0 {
			continue
		}
		if _, dup := seen[port.VMUUID]; dup {
			continue
		}
		seen[port.VMUUID] = struct{}{}
		out = append(out, port.VMUUID)
	}
	return out
}

// portReferencesSG decides whether a port is affected by sgUUID,
// applying the same fallback rule the resolver uses : explicit
// per-port list wins, empty list means inherit from the network.
func portReferencesSG(snap Snapshot, port weft.Port, sgUUID string) bool {
	if len(port.SecurityGroups) > 0 {
		return containsSG(port.SecurityGroups, sgUUID)
	}
	if net, ok := snap.NetworkByUUID(port.NetworkUUID); ok {
		return containsSG(net.DefaultSecurityGroups, sgUUID)
	}
	return false
}

// JSONPublishFunc is a small helper that wires PublishFunc onto any
// NATS-like transport with a `Publish(subject string, data []byte)
// error` method. The agent's Subject is "weft.firewall.<vm-uuid>",
// matching pkg/firewall.Subject on the guest side.
func JSONPublishFunc(nc Conn) PublishFunc {
	return func(vmUUID string, fw pod.Firewall) error {
		data, err := json.Marshal(fw)
		if err != nil {
			return fmt.Errorf("marshal firewall: %w", err)
		}
		return nc.Publish("weft.firewall."+vmUUID, data)
	}
}

// Conn is the narrow transport interface JSONPublishFunc needs. The
// *nats.Conn returned by NATSEventBus.Conn() satisfies this without
// any further wrapping. Kept as a named interface so tests can swap
// in an in-memory recorder without dragging in the nats client.
type Conn interface {
	Publish(subject string, data []byte) error
}
