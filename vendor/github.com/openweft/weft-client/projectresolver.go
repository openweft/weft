// Package weftclient — projectresolver.go is a small in-memory
// cache of (project UUID → display name) that the event streamer
// uses to swap raw UUIDs for readable names in human output.
//
// The resolver is self-updating: it bootstraps with a single
// ListProjects RPC at stream open, then watches every
// `project.created` / `project.renamed` / `project.deleted`
// event flowing through the same WatchEvents stream and updates
// its map in place. So a project rename mid-stream is reflected
// in subsequent rows without re-polling weft.
//
// Thread-safe — concurrent reads from the render goroutine and
// updates from the stream goroutine are guarded by a mutex.
// Unknown UUIDs return the UUID itself; better one piece of
// stable identification than an empty cell.
package weftclient

import (
	"context"
	"sync"

	weftv1 "github.com/openweft/weft-proto"
)

// ProjectResolver maps project UUID → display name. Zero value is
// usable but empty (every Name lookup returns the input UUID
// verbatim); callers typically bootstrap via NewProjectResolver.
type ProjectResolver struct {
	mu   sync.RWMutex
	byID map[string]string
}

// NewProjectResolver opens an empty resolver. Call Bootstrap to
// fill it before use; Apply to keep it current.
func NewProjectResolver() *ProjectResolver {
	return &ProjectResolver{byID: make(map[string]string)}
}

// Bootstrap fills the cache from a ListProjects RPC. Best-effort:
// a failed list logs nothing and leaves the cache empty — the
// streamer keeps working, just without name resolution.
func (r *ProjectResolver) Bootstrap(ctx context.Context, c weftv1.WeftAgentClient) {
	resp, err := c.ListProjects(ctx, &weftv1.ListProjectsRequest{})
	if err != nil {
		return
	}
	r.mu.Lock()
	for _, p := range resp.Projects {
		r.byID[p.Uuid] = p.Name
	}
	r.mu.Unlock()
}

// Apply updates the cache from a single PlatformEvent. Watches
// the three `project.*` kinds; everything else is a no-op so the
// streamer can call this for every event without per-event
// dispatch.
//
// Caller already verified the event is non-nil (the stream
// always emits non-nil; defensive guard inside is just polish).
func (r *ProjectResolver) Apply(ev *weftv1.PlatformEvent) {
	if ev == nil || ev.ProjectUuid == "" {
		return
	}
	switch ev.Kind {
	case "project.created", "project.renamed":
		// Both events carry the post-mutation name in Meta.
		var name string
		if ev.Meta != nil {
			// Created uses Meta["name"]; Renamed uses Meta["new_name"].
			// Try both — order matters because the empty value of
			// either should not stomp a real one from the other.
			if v, ok := ev.Meta["new_name"]; ok && v != "" {
				name = v
			} else if v, ok := ev.Meta["name"]; ok && v != "" {
				name = v
			}
		}
		if name == "" {
			return
		}
		r.mu.Lock()
		// On rename, the old (uuid → old-name) entry stays — same
		// UUID, new name overwrites the value. No stale entries to
		// clean.
		r.byID[ev.ProjectUuid] = name
		r.mu.Unlock()
	case "project.deleted":
		r.mu.Lock()
		delete(r.byID, ev.ProjectUuid)
		r.mu.Unlock()
	}
}

// Name returns the display name for `uuid` or the UUID itself
// when unknown. Stable identity beats a blank.
func (r *ProjectResolver) Name(uuid string) string {
	if r == nil || uuid == "" {
		return uuid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n, ok := r.byID[uuid]; ok {
		return n
	}
	return uuid
}
