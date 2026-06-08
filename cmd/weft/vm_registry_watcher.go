package main

// vm_registry_watcher.go wires Adapter.WatchVMRegistry to the daemon
// lifecycle. Same one-defer pattern as floating_ip_nat.go +
// respawn_subscriber.go : the hook returns a cancel that the caller
// runs LIFO via defer.

import (
	"context"
	"log"

	weft "github.com/openweft/weft"
)

// startVMRegistryWatcher launches the background goroutine that
// reloads the in-memory vm registry on every remote PUT/DELETE.
// Returns a cancel that stops the watcher cleanly. Always non-nil ;
// no-op when the underlying Storage isn't etcd-backed.
func startVMRegistryWatcher(adp weft.VZAdapter, logger *log.Logger) func() {
	a, ok := adp.(interface {
		WatchVMRegistry(context.Context)
	})
	if !ok {
		// VZAdapter doesn't expose WatchVMRegistry on this build —
		// nothing to start, nothing to cancel.
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.WatchVMRegistry(ctx)
	logger.Printf("vm registry watcher: subscribed (reload on remote put/delete)")
	return cancel
}
