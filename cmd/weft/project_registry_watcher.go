package main

// project_registry_watcher.go wires Adapter.WatchProjectRegistry to
// the daemon lifecycle. Same one-defer pattern as
// vm_registry_watcher.go : the hook returns a cancel that the caller
// runs LIFO via defer.

import (
	"context"
	"log"

	weft "github.com/openweft/weft"
)

// startProjectRegistryWatcher launches the background goroutine
// that reloads the in-memory project registry on every remote
// PUT/DELETE. Returns a cancel that stops the watcher cleanly.
// Always non-nil ; no-op when the underlying Storage isn't etcd-
// backed.
func startProjectRegistryWatcher(adp weft.VZAdapter, logger *log.Logger) func() {
	a, ok := adp.(interface {
		WatchProjectRegistry(context.Context)
	})
	if !ok {
		// VZAdapter doesn't expose WatchProjectRegistry on this build —
		// nothing to start, nothing to cancel.
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.WatchProjectRegistry(ctx)
	logger.Printf("project registry watcher: subscribed (reload on remote put/delete)")
	return cancel
}
