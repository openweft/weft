package floatingipl2

import (
	"context"
	"log"

	weft "github.com/openweft/weft"
)

// Watcher is the host-side reactor for the L2/VLAN attachment
// path : subscribes to platform events, recomputes the local
// macvlan + ARP set, calls Programmer.Apply.
//
// Symmetric to floatingipnat.Watcher — same event kinds trigger
// a reconcile, same initial-sync pattern. The two reactors run
// in parallel on every weft-agent : NAT rewrites the packet,
// L2 makes the packet arrive in the first place. For BGP-mode
// networks the L2 reactor produces an empty set ; for VLAN-mode
// networks the BGP path is irrelevant. They never overlap on the
// same FIP.
//
// One Watcher per weft-agent process. Doesn't keep state itself
// — the canonical state is the registry, the watcher just reads
// and projects to the kernel.
type Watcher struct {
	hostUUID   string
	scope      Scope
	programmer Programmer
	logger     *log.Logger
}

// New builds a Watcher. nil logger → log.Default.
func New(hostUUID string, scope Scope, programmer Programmer, logger *log.Logger) *Watcher {
	if logger == nil {
		logger = log.Default()
	}
	return &Watcher{hostUUID: hostUUID, scope: scope, programmer: programmer, logger: logger}
}

// Run consumes events until ctx is cancelled. Each relevant kind
// triggers a full recompute + Apply ; irrelevant kinds are
// silently dropped. Apply errors log + skip — the next relevant
// event self-heals.
func (w *Watcher) Run(ctx context.Context, events <-chan weft.PlatformEvent) error {
	// Initial sync : drive the programmer once at startup so a
	// freshly-restarted weft-agent picks up every L2 attachment
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

// shouldReact returns true when ev invalidates our L2 set.
// Same event taxonomy as floatingipnat.Watcher : FIP lifecycle,
// VM moves, network reconfigurations. Pure ; testable.
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
	case "network.updated":
		// A network flipping from bgp to vlan (or vice versa)
		// changes whether its FIPs need L2 ; re-reconcile.
		return true
	}
	return false
}

func (w *Watcher) reconcile() {
	mappings := ComputeLocalL2Mappings(w.scope, w.hostUUID)
	if err := w.programmer.Apply(mappings); err != nil {
		w.logger.Printf("floatingipl2: apply: %v", err)
		return
	}
	w.logger.Printf("floatingipl2: applied %d L2 mapping(s) on host %s", len(mappings), w.hostUUID)
}
