package floatingipnat

import (
	"context"
	"log"

	weft "github.com/openweft/weft"
)

// Scope is the narrow read-only adapter view the watcher needs.
// The production *weft.Adapter satisfies it ; tests inject
// hand-rolled stubs. Kept narrow on purpose so a future migration
// off the adapter only needs to re-implement these methods.
type Scope interface {
	// ListVMsForHost returns every VM scheduled on hostUUID. Used
	// by the watcher to decide which floating IPs need a local
	// NAT rule.
	ListVMsForHost(hostUUID string) []weft.VM
	// ListFloatingIPs returns every floating IP across all
	// projects ; the watcher filters by mapped_to ∈ local VMs.
	ListFloatingIPs() []weft.FloatingIP
	// ListPortsForVM resolves a VM's NICs ; the watcher picks
	// one to use as the NAT private IP (today : first by UUID
	// sort ; tomorrow : the one whose network matches the FIP's
	// project gateway).
	ListPortsForVM(vmUUID string) []weft.Port
}

// Watcher is the host-side reactor : subscribes to platform
// events, recomputes every local VM's floating-IP NAT mappings,
// and calls the Reconciler.Apply.
//
// One Watcher per weft-agent process. Doesn't keep state itself —
// the canonical state is the registry, the watcher just reads it
// and projects to nftables. Whole-state replace on every relevant
// event keeps the model simple ; a missed event self-heals on the
// next one.
type Watcher struct {
	hostUUID   string
	scope      Scope
	reconciler Reconciler
	logger     *log.Logger
}

// New builds a Watcher. nil logger → log.Default.
func New(hostUUID string, scope Scope, reconciler Reconciler, logger *log.Logger) *Watcher {
	if logger == nil {
		logger = log.Default()
	}
	return &Watcher{hostUUID: hostUUID, scope: scope, reconciler: reconciler, logger: logger}
}

// Run consumes events until ctx is cancelled. Each relevant kind
// triggers a full recompute + Apply ; irrelevant kinds are
// silently dropped. Apply errors log + skip — the next relevant
// event self-heals.
func (w *Watcher) Run(ctx context.Context, events <-chan weft.PlatformEvent) error {
	// Initial sync : drives the table once at startup so a
	// freshly-restarted weft-agent picks up every mapping
	// without waiting for the next event.
	w.reconcile()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if !w.shouldReact(ev) {
				continue
			}
			w.reconcile()
		}
	}
}

// shouldReact decides whether this event invalidates our current
// nftables table. Pure ; testable.
func (w *Watcher) shouldReact(ev weft.PlatformEvent) bool {
	switch ev.Kind {
	case "floating_ip.mapped",
		"floating_ip.unmapped",
		"floating_ip.released":
		return true
	case "vm.created",
		"vm.deleted",
		"vm.migrated",
		"vm.state_changed":
		return true
	case "port.created",
		"port.deleted":
		// A port change can shift the private IP we'd NAT to.
		return true
	}
	return false
}

// reconcile is the recompute-+-Apply path. Pulled out so the
// initial-sync call and the event-driven call share one
// implementation.
func (w *Watcher) reconcile() {
	mappings := ComputeLocalMappings(w.scope, w.hostUUID)
	if err := w.reconciler.Apply(mappings); err != nil {
		w.logger.Printf("floatingipnat: apply: %v", err)
		return
	}
	w.logger.Printf("floatingipnat: applied %d mapping(s) on host %s", len(mappings), w.hostUUID)
}

// ComputeLocalMappings is the pure projection from "adapter
// snapshot" → "NAT mappings for THIS host". Returns ONE entry per
// active FIP whose target VM has a port with an IP — regardless of
// whether the VM is currently scheduled on this host.
//
// Broad-coverage rationale : with weft-router multi-replica HA
// (Router.Replicas ≥ 2 + BGP multipath upstream), any of the N
// replica hosts can receive inbound traffic for a given /32 ; the
// packet must be DNAT-able there even when the target VM lives
// elsewhere. The kernel then routes the post-DNAT packet via the
// mesh underlay to the host where the VM actually runs.
//
// For single-replica routers (Replicas == 1) the broader-than-needed
// rules are dead weight on most hosts but harmless — the DNAT only
// fires when a matching public IP arrives, which only happens on
// the host the upstream actually sent the packet to.
//
// Migration latency : with broad coverage, a VM moving from H1 to
// H2 finds its DNAT rule already on H2 — no NAT install delay,
// the only failover window is BGP redistribution (one keepalive)
// or gARP propagation (ms) for VLAN-mode networks. Pre-install.
//
// VMs without a port-assigned IP yet (still booting) drop out ;
// the next port.created event re-reconciles. Multi-NIC VMs pick
// the lowest-UUID port deterministically — same rule as before.
func ComputeLocalMappings(scope Scope, hostUUID string) []NATMapping {
	var out []NATMapping
	for _, fip := range scope.ListFloatingIPs() {
		if fip.Status != weft.FIPStatusActive || fip.TargetKind != weft.FIPTargetVM {
			continue
		}
		vmUUID, vmName := resolveTargetVM(scope, hostUUID, fip.MappedTo)
		if vmUUID == "" {
			continue
		}
		privateIP := firstPortIP(scope, vmUUID)
		if privateIP == "" {
			continue
		}
		out = append(out, NATMapping{
			PublicIP:     fip.Address,
			PrivateIP:    privateIP,
			VMName:       vmName,
			RateLimitPPS: fip.RateLimitPPS,
		})
	}
	return out
}

// resolveTargetVM returns (UUID, Name) for the VM matching vmName
// the FIP is mapped to. Behaviour :
//
//   - Scope-impls exposing ListHostUUIDs (production *weft.Adapter)
//     get broad coverage : walk every host's VMs, return the match.
//     The NAT rule is installed on every host because any of them
//     can receive inbound traffic when the Router has Replicas ≥ 2
//     with BGP multipath upstream.
//   - Minimal Scope-impls (tests, future-proofed) fall back to the
//     local host only (preserving the pre-broad-coverage behaviour).
//
// Linear in (host count × VM count) ; fine for typical clusters
// (few hundred VMs at most). The kernel-side cost is N×M nftables
// rules, harmless on hosts that never receive the matching packet.
func resolveTargetVM(scope Scope, hostUUID, vmName string) (uuid, name string) {
	if vmName == "" {
		return "", ""
	}
	// hostsLister is the opt-in widening for production.
	type hostsLister interface {
		ListHostUUIDs() []string
	}
	if hl, ok := scope.(hostsLister); ok {
		for _, h := range hl.ListHostUUIDs() {
			for _, vm := range scope.ListVMsForHost(h) {
				if vm.Name == vmName {
					return vm.UUID, vm.Name
				}
			}
		}
		return "", ""
	}
	// Fallback : local host only — pre-broad-coverage behaviour.
	for _, vm := range scope.ListVMsForHost(hostUUID) {
		if vm.Name == vmName {
			return vm.UUID, vm.Name
		}
	}
	return "", ""
}

// firstPortIP returns the lowest-UUID port's IP for vmUUID, or
// "" when the VM has no ports yet. Stable across reconciles : we
// rely on the adapter's deterministic sort (portRegistry sorts by
// IP in listForVM, but we want a stable tie-breaker independent
// of allocation order — so we just take the first entry).
func firstPortIP(scope Scope, vmUUID string) string {
	ports := scope.ListPortsForVM(vmUUID)
	for _, p := range ports {
		if p.IP != "" {
			return p.IP
		}
	}
	return ""
}
