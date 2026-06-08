package main

// scheduling_rule_registry_watcher.go wires
// Adapter.WatchSchedulingRuleRegistry to the daemon lifecycle. Same
// one-defer pattern as vm_registry_watcher.go : the hook returns a
// cancel that the caller runs LIFO via defer.

import (
	"context"
	"log"

	weft "github.com/openweft/weft"
)

// startSchedulingRuleRegistryWatcher launches the background
// goroutine that reloads the in-memory scheduling rule registry on
// every remote PUT/DELETE. Returns a cancel that stops the watcher
// cleanly. Always non-nil ; no-op when the underlying Storage isn't
// etcd-backed.
func startSchedulingRuleRegistryWatcher(adp weft.VZAdapter, logger *log.Logger) func() {
	a, ok := adp.(interface {
		WatchSchedulingRuleRegistry(context.Context)
	})
	if !ok {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.WatchSchedulingRuleRegistry(ctx)
	logger.Printf("scheduling rule registry watcher: subscribed (reload on remote put/delete)")
	return cancel
}
