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
	// VMByName resolves a name to its UUID — Map's target_name is
	// stored as VM name, not UUID, so we need this hop.
	VMByName(name string) (weft.VM, bool)
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

// ComputeLocalMappings is the pure projection from
// "adapter snapshot" → "NAT mappings for THIS host". Walks every
// FloatingIP, drops the ones not mapped to a local VM, resolves
// the VM's private IP via the first port on it.
//
// Multi-NIC VMs : v0 picks the lowest-UUID port deterministically.
// A future revision can let MapFloatingIP carry an explicit port
// UUID so the operator targets a specific NIC.
//
// VMs without an IP yet (still booting) drop out of the result ;
// the next port.created event re-reconciles.
func ComputeLocalMappings(scope Scope, hostUUID string) []NATMapping {
	// Build the local-VM set first so we can short-circuit FIPs
	// pointed at remote VMs without per-FIP adapter calls.
	localVMByName := make(map[string]weft.VM)
	for _, vm := range scope.ListVMsForHost(hostUUID) {
		localVMByName[vm.Name] = vm
	}
	if len(localVMByName) == 0 {
		return nil
	}

	var out []NATMapping
	for _, fip := range scope.ListFloatingIPs() {
		if fip.Status != weft.FIPStatusActive || fip.TargetKind != weft.FIPTargetVM {
			continue
		}
		vm, local := localVMByName[fip.MappedTo]
		if !local {
			continue
		}
		privateIP := firstPortIP(scope, vm.UUID)
		if privateIP == "" {
			continue
		}
		out = append(out, NATMapping{
			PublicIP:  fip.Address,
			PrivateIP: privateIP,
			VMName:    vm.Name,
		})
	}
	return out
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
