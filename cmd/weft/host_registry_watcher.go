package main

// host_registry_watcher.go wires Adapter.WatchHostRegistry to the
// daemon lifecycle. Same one-defer pattern as
// vm_registry_watcher.go + project_registry_watcher.go.

import (
	"context"
	"log"

	weft "github.com/openweft/weft"
)

// startHostRegistryWatcher launches the background goroutine that
// applies per-record host events (heartbeat, state changes, label
// edits) coming from other DCs surgically to this agent's
// in-memory hostRegistry. Returns a cancel that stops the watcher
// cleanly ; always non-nil ; no-op on blob backends.
func startHostRegistryWatcher(adp weft.VZAdapter, logger *log.Logger) func() {
	a, ok := adp.(interface {
		WatchHostRegistry(context.Context)
	})
	if !ok {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.WatchHostRegistry(ctx)
	logger.Printf("host registry watcher: subscribed (reload on remote put/delete)")
	return cancel
}
