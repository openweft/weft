package main

// floating_ip_nat.go wires floatingipnat.Watcher into the daemon :
// subscribes to floating_ip.* / vm.* / port.* events on the existing
// platform event bus and drives the per-host nftables NAT reconciler.
//
// Linux : real netlink path via floatingipnat.NewLinuxReconciler().
// Darwin / other : StubReconciler — records the desired state for
// host-side tests but doesn't touch any kernel (weft-agent never
// runs in production off Linux, but the cross-platform dev build
// has to stay green).
//
// Kept in its own file so the call-site in main.go is one defer
// and the wiring is easy to disable / replace without churning
// the rest of cmd/weft.

import (
	"context"
	"log"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft/floatingipnat"
)

// startFloatingIPNATWatcher starts the host-side NAT reconciler
// loop. Returns a cancel that stops the goroutine + drops the
// bus subscription cleanly. Always returns a non-nil cancel (the
// daemon shutdown path calls it unconditionally) ; an
// initialisation failure logs + returns a no-op.
func startFloatingIPNATWatcher(adp weft.VZAdapter, bus weft.EventBus, logger *log.Logger) func() {
	hostUUID := localHostUUID(adp)
	if hostUUID == "" {
		logger.Printf("floating-ip NAT watcher: no local host UUID (selfRegister failed earlier ?) — skipping")
		return func() {}
	}

	scope := watcherScope{adp: adp}
	rec := newFloatingIPNATReconciler()
	w := floatingipnat.New(hostUUID, scope, rec, logger)

	// SeeAll = true : platform-internal consumer, not a tenant ;
	// needs every event regardless of project visibility. The
	// prefix narrows are wide on purpose — Watcher.shouldReact
	// re-filters anyway, but doing it server-side keeps the
	// per-event wakeup cost low.
	events, cancelSub := bus.Subscribe(weft.EventFilter{
		KindPrefixes: []string{"floating_ip.", "vm.", "port."},
		SeeAll:       true,
	})

	ctx, cancelCtx := context.WithCancel(context.Background())
	go func() {
		if err := w.Run(ctx, events); err != nil && err != context.Canceled {
			logger.Printf("floating-ip NAT watcher exited: %v", err)
		}
	}()
	logger.Printf("floating-ip NAT watcher: host=%s, subscribed (floating_ip./vm./port.)", hostUUID)
	return func() {
		cancelSub()
		cancelCtx()
	}
}

// watcherScope adapts the production *weft.Adapter (via the
// VZAdapter interface) to floatingipnat.Scope. The 4 method
// signatures line up exactly — this wrapper exists only to avoid
// adding a direct floatingipnat import to the root weft package
// (the dep direction stays floatingipnat → weft, not the other way).
type watcherScope struct{ adp weft.VZAdapter }

func (s watcherScope) ListVMsForHost(hostUUID string) []weft.VM {
	return s.adp.ListVMsForHost(hostUUID)
}
func (s watcherScope) ListFloatingIPs() []weft.FloatingIP {
	return s.adp.ListFloatingIPs()
}
func (s watcherScope) ListPortsForVM(vmUUID string) []weft.Port {
	return s.adp.ListPortsForVM(vmUUID)
}
