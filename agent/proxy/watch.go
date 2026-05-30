package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Watcher streams route updates from etcd and pushes them through a
// Supervisor.Apply. One Watcher per host — the etcd key it watches is
// `<KeyPrefix>/<host>` (one JSON document carrying the full Routes slice).
//
// Why one key per host (not per-route): Caddy's admin /load endpoint replaces
// the entire config atomically. Streaming per-route deltas would require
// merging on our side anyway. One blob keeps the etcd write side simple
// (`weft up` writes the whole table) and lets us short-circuit byte-identical
// reloads inside Supervisor.Apply.
//
// Debounce: a `weft up` apply may write the key multiple times in quick
// succession (HCL → etcd is one write per host but the etcd watcher
// occasionally surfaces a put twice — once on lease assignment, once on
// the actual value). 200ms is enough to coalesce without making
// operator-driven changes feel sluggish.
type Watcher struct {
	Client        *clientv3.Client
	KeyPrefix     string // default "/weft/proxy/routes"
	HostID        string // this host's UUID — `/<KeyPrefix>/<HostID>` is the watched key
	Supervisor    *Supervisor
	DebounceWait  time.Duration // default 200ms
}

// Run blocks until ctx is cancelled. It performs:
//
//  1. An initial Get of the watched key — applies whatever's already there
//     so a freshly-started agent doesn't need to wait for the next operator
//     change to install the current route table.
//  2. A long-lived Watch over the same key, debounced; every settled
//     batch ends in an Apply.
//
// Etcd watch streams reconnect transparently inside clientv3; we don't
// need a manual retry loop. Errors from a closed stream surface via
// ctx.Done — there's no separate error return.
func (w *Watcher) Run(ctx context.Context) error {
	if w.Client == nil {
		return errors.New("proxy.Watcher: Client is required")
	}
	if w.Supervisor == nil {
		return errors.New("proxy.Watcher: Supervisor is required")
	}
	if w.HostID == "" {
		return errors.New("proxy.Watcher: HostID is required")
	}
	if w.KeyPrefix == "" {
		w.KeyPrefix = "/weft/proxy/routes"
	}
	if w.DebounceWait == 0 {
		w.DebounceWait = 200 * time.Millisecond
	}
	key := w.KeyPrefix + "/" + w.HostID

	// Initial read.
	if resp, err := w.Client.Get(ctx, key); err == nil && len(resp.Kvs) > 0 {
		if err := w.applyValue(ctx, resp.Kvs[0].Value); err != nil {
			log.Printf("weft-agent proxy: initial apply: %v", err)
		}
	} else if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("weft-agent proxy: initial Get(%s): %v", key, err)
	}

	// Watch from the revision after the initial Get (or HEAD if nothing
	// was there). clientv3 internally reconnects on transient failures
	// — the channel only closes on ctx done or a fatal stream error.
	ch := w.Client.Watch(ctx, key)
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	var pending []byte
	hasPending := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return errors.New("etcd watch channel closed")
			}
			if err := ev.Err(); err != nil {
				log.Printf("weft-agent proxy: watch err: %v (continuing)", err)
				continue
			}
			for _, e := range ev.Events {
				switch e.Type {
				case clientv3.EventTypePut:
					pending = append(pending[:0], e.Kv.Value...)
					hasPending = true
				case clientv3.EventTypeDelete:
					// Route table deleted → apply an empty
					// table (Caddy idle, accepts no routes).
					pending = []byte("[]")
					hasPending = true
				}
			}
			if hasPending {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounce.Reset(w.DebounceWait)
			}
		case <-debounce.C:
			if !hasPending {
				continue
			}
			value := pending
			pending = nil
			hasPending = false
			if err := w.applyValue(ctx, value); err != nil {
				log.Printf("weft-agent proxy: apply: %v", err)
			}
		}
	}
}

// applyValue decodes the etcd value (a JSON `Routes` array) and pushes it
// to the supervisor. A bad payload is surfaced as a log line, not a
// shutdown — a malformed etcd write shouldn't take down the agent's
// reverse-proxy plane.
func (w *Watcher) applyValue(ctx context.Context, value []byte) error {
	var routes Routes
	if err := json.Unmarshal(value, &routes); err != nil {
		return fmt.Errorf("decode routes: %w", err)
	}
	return w.Supervisor.Apply(ctx, routes)
}
