// Package etcdcoord is the cross-host coordination layer the
// respawn V0.1.2 HA story rests on. Three building blocks :
//
//   - HostLiveness : every weft agent registers a 10s-TTL lease at
//     /weft/coord/hosts/<host_uuid> and refreshes it every 3s.
//     Lease expiry signals the host is down (process died, network
//     partition, kernel panic). Other agents watching the prefix
//     pick up the DELETE event and react.
//
//   - HostWatcher : subscribes to the prefix and emits HostEvent
//     {Kind: Up|Down, HostUUID, Metadata} on a channel. The
//     agentrespawn subscriber consumes this to spot orphaned VMs.
//
//   - Election : etcd-concurrency leader election scoped to a key
//     prefix. The respawn reconciler uses one election per
//     SchedulingRule, so cross-host claim of an orphan's VMs is
//     coalesced to a single agent and we don't get N-way StartVM
//     thrash.
//
// All three are pure-Go, CGO=0 ; the only dep is the already-
// vendored go.etcd.io/etcd/client/v3 + .../concurrency. The
// package leaves the etcd dial to the caller (the agent already
// holds an open *clientv3.Client) so we don't fan out connections.
package etcdcoord

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

const (
	// HostsPrefix is the etcd key prefix under which each agent
	// registers its liveness lease. Caller can override via
	// LivenessOptions.Prefix when running multiple weft fleets
	// against a single etcd cluster.
	HostsPrefix = "/weft/coord/hosts/"

	// DefaultLeaseTTLSec is the etcd lease TTL. 10s gives us a
	// 7-13s window between a host dying and other agents
	// observing it — short enough to drive HA failover, long
	// enough to absorb a single missed refresh due to GC pause.
	DefaultLeaseTTLSec = 10

	// DefaultRefreshInterval is how often a healthy HostLiveness
	// renews its lease. Set to TTL/3 so a single dropped renew
	// doesn't expire the lease (similar logic as the sd_notify
	// watchdog half-period rule).
	DefaultRefreshInterval = 3 * time.Second
)

// HostMetadata is the JSON value stored under each host's liveness
// key. Keep it small — every agent watching the prefix decodes it.
type HostMetadata struct {
	HostUUID   string `json:"host_uuid"`
	Hostname   string `json:"hostname"`
	Hypervisor string `json:"hypervisor"`
	Version    string `json:"version,omitempty"`
	StartedAt  int64  `json:"started_at_unix_ns"`
}

// ---- HostLiveness --------------------------------------------------

// LivenessOptions configures a HostLiveness registration. All fields
// are optional ; the zero value uses package defaults.
type LivenessOptions struct {
	Prefix      string        // defaults to HostsPrefix
	LeaseTTLSec int64         // defaults to DefaultLeaseTTLSec
	Refresh     time.Duration // defaults to DefaultRefreshInterval
	Logger      *slog.Logger  // defaults to a discard handler
}

// HostLiveness holds the lease that announces "this agent is alive"
// to the cluster. Stop() is idempotent + closes cleanly so a
// graceful shutdown deregisters immediately instead of waiting for
// TTL to expire.
type HostLiveness struct {
	cli      *clientv3.Client
	key      string
	leaseID  clientv3.LeaseID
	cancel   context.CancelFunc
	stopOnce sync.Once
	log      *slog.Logger
	done     chan struct{}
}

// RegisterHostLiveness grants an etcd lease + attaches the host
// metadata, then launches a goroutine that keeps the lease alive
// until ctx is cancelled or Stop() is called. Returns the live
// HostLiveness handle so the caller can call Stop() + Wait().
//
// Failure modes :
//   - etcd dial / grant errors return immediately.
//   - keepalive channel close mid-flight is logged and the lease
//     allowed to expire (caller should detect via the parent ctx).
func RegisterHostLiveness(ctx context.Context, cli *clientv3.Client, meta HostMetadata, opts LivenessOptions) (*HostLiveness, error) {
	if cli == nil {
		return nil, fmt.Errorf("etcdcoord: nil etcd client")
	}
	if meta.HostUUID == "" {
		return nil, fmt.Errorf("etcdcoord: HostUUID is required")
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = HostsPrefix
	}
	ttl := opts.LeaseTTLSec
	if ttl <= 0 {
		ttl = DefaultLeaseTTLSec
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardW{}, &slog.HandlerOptions{Level: slog.LevelError}))
	}

	grant, err := cli.Grant(ctx, ttl)
	if err != nil {
		return nil, fmt.Errorf("etcdcoord: grant lease: %w", err)
	}
	if meta.StartedAt == 0 {
		// Caller-provided start time wins. Default to "now" only when
		// not set ; this matters for tests that pin time.
		meta.StartedAt = nowNanos()
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		_, _ = cli.Revoke(ctx, grant.ID)
		return nil, fmt.Errorf("etcdcoord: marshal metadata: %w", err)
	}
	key := path.Join(prefix, meta.HostUUID)
	if _, err := cli.Put(ctx, key, string(blob), clientv3.WithLease(grant.ID)); err != nil {
		_, _ = cli.Revoke(ctx, grant.ID)
		return nil, fmt.Errorf("etcdcoord: put liveness key: %w", err)
	}

	keepCtx, cancel := context.WithCancel(context.Background())
	hl := &HostLiveness{
		cli:     cli,
		key:     key,
		leaseID: grant.ID,
		cancel:  cancel,
		log:     log,
		done:    make(chan struct{}),
	}

	keepCh, err := cli.KeepAlive(keepCtx, grant.ID)
	if err != nil {
		cancel()
		_, _ = cli.Revoke(ctx, grant.ID)
		close(hl.done)
		return nil, fmt.Errorf("etcdcoord: keepalive: %w", err)
	}
	go func() {
		defer close(hl.done)
		for {
			select {
			case <-keepCtx.Done():
				return
			case _, ok := <-keepCh:
				if !ok {
					log.Warn("etcdcoord: keepalive channel closed ; lease will expire", "key", key)
					return
				}
				// Successful refresh — nothing to log at default level.
			}
		}
	}()
	log.Info("etcdcoord: host liveness registered", "key", key, "ttl_sec", ttl, "lease_id", grant.ID)
	return hl, nil
}

// Stop revokes the lease (immediate deregister, no TTL wait) and
// shuts the keepalive goroutine down. Idempotent ; safe to call
// from a defer.
func (h *HostLiveness) Stop(ctx context.Context) error {
	var rerr error
	h.stopOnce.Do(func() {
		h.cancel()
		if _, err := h.cli.Revoke(ctx, h.leaseID); err != nil {
			rerr = fmt.Errorf("etcdcoord: revoke lease: %w", err)
			return
		}
		h.log.Info("etcdcoord: host liveness deregistered", "key", h.key)
	})
	<-h.done
	return rerr
}

// LeaseID returns the underlying etcd lease the registration is
// bound to. Useful for tests + diagnostics ; callers should not
// revoke it directly (use Stop()).
func (h *HostLiveness) LeaseID() clientv3.LeaseID { return h.leaseID }

// Key returns the etcd key under which the liveness lease was put.
func (h *HostLiveness) Key() string { return h.key }

// ---- HostWatcher ---------------------------------------------------

// HostEventKind is the discriminator on a HostEvent.
type HostEventKind int

const (
	HostUp   HostEventKind = iota // new agent registered (PUT on its key)
	HostDown                      // lease expired or agent revoked (DELETE)
)

// HostEvent is one observation about a cluster member.
type HostEvent struct {
	Kind     HostEventKind
	HostUUID string
	Metadata HostMetadata // populated on Up ; zero on Down
}

// WatcherOptions configures a HostWatcher.
type WatcherOptions struct {
	Prefix    string       // defaults to HostsPrefix
	Logger    *slog.Logger // defaults to discard
	IncludeSelf string     // if non-empty, suppress events for this HostUUID
}

// HostWatcher emits HostEvents on its channel until ctx is cancelled.
// Closes the channel cleanly on exit so consumers can `for range`.
//
// The initial Get-with-prefix returns every existing host as a
// synthetic HostUp event so a freshly-started agent gets a baseline
// view of the cluster without waiting for renew events.
type HostWatcher struct {
	cli  *clientv3.Client
	opts WatcherOptions
	ch   chan HostEvent
	wg   sync.WaitGroup
	log  *slog.Logger
}

// NewHostWatcher starts watching ; the returned channel receives events
// until ctx is cancelled. The channel is buffered to 32 — fast enough
// for fleets up to ~thousands of hosts where the events of interest
// are sparse.
func NewHostWatcher(ctx context.Context, cli *clientv3.Client, opts WatcherOptions) (*HostWatcher, error) {
	if cli == nil {
		return nil, fmt.Errorf("etcdcoord: nil etcd client")
	}
	if opts.Prefix == "" {
		opts.Prefix = HostsPrefix
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(discardW{}, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	w := &HostWatcher{
		cli:  cli,
		opts: opts,
		ch:   make(chan HostEvent, 32),
		log:  opts.Logger,
	}

	// Phase 1 : snapshot via prefix Get, emit synthetic Up events for
	// each existing host. WithSerializable keeps the call cheap — we
	// don't need linearizable consistency here ; the watch below picks
	// up any miss.
	resp, err := cli.Get(ctx, opts.Prefix, clientv3.WithPrefix(), clientv3.WithSerializable())
	if err != nil {
		return nil, fmt.Errorf("etcdcoord: initial get: %w", err)
	}
	for _, kv := range resp.Kvs {
		if ev, ok := w.makeEvent(kv.Key, kv.Value, HostUp); ok {
			w.deliver(ctx, ev)
		}
	}

	// Phase 2 : watch the prefix from the snapshot's revision so we
	// don't miss any event between the Get and the Watch.
	rev := resp.Header.Revision + 1
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer close(w.ch)
		ch := cli.Watch(ctx, opts.Prefix, clientv3.WithPrefix(), clientv3.WithRev(rev), clientv3.WithPrevKV())
		for wr := range ch {
			if wr.Err() != nil {
				w.log.Warn("etcdcoord: watch error", "err", wr.Err())
				continue
			}
			for _, ev := range wr.Events {
				kind := HostUp
				keySrc := ev.Kv.Key
				valSrc := ev.Kv.Value
				if ev.Type.String() == "DELETE" {
					kind = HostDown
					if ev.PrevKv != nil {
						// Deliver Down with the last-seen metadata so the
						// consumer can name the host without a separate
						// inventory lookup.
						valSrc = ev.PrevKv.Value
					} else {
						valSrc = nil
					}
				}
				if hev, ok := w.makeEvent(keySrc, valSrc, kind); ok {
					w.deliver(ctx, hev)
				}
			}
		}
	}()

	return w, nil
}

// Events returns the channel that fires HostEvents. Closes when the
// watcher's context is cancelled.
func (w *HostWatcher) Events() <-chan HostEvent { return w.ch }

// Wait blocks until the watcher's goroutine exits. Useful in tests
// + clean shutdown.
func (w *HostWatcher) Wait() { w.wg.Wait() }

func (w *HostWatcher) makeEvent(key, val []byte, kind HostEventKind) (HostEvent, bool) {
	uuid := path.Base(string(key))
	if uuid == "" || uuid == "/" {
		return HostEvent{}, false
	}
	if uuid == w.opts.IncludeSelf {
		return HostEvent{}, false
	}
	ev := HostEvent{Kind: kind, HostUUID: uuid}
	if len(val) > 0 {
		var meta HostMetadata
		if err := json.Unmarshal(val, &meta); err == nil {
			ev.Metadata = meta
		}
	}
	return ev, true
}

func (w *HostWatcher) deliver(ctx context.Context, ev HostEvent) {
	select {
	case w.ch <- ev:
	case <-ctx.Done():
	}
}

// ---- Leader election ----------------------------------------------

// Election wraps etcd-concurrency's election with a Campaign helper
// that times out cleanly. One Election per SchedulingRule means
// orphan-claim work coalesces to a single agent per rule, avoiding
// N-way StartVM thrash on a host-down event.
type Election struct {
	session  *concurrency.Session
	election *concurrency.Election
	owned    bool
	log      *slog.Logger
	key      string
}

// ElectionOptions configures a leader election.
type ElectionOptions struct {
	Key      string        // etcd prefix the election locks on, e.g. "/weft/coord/elect/respawn/<rule_uuid>"
	TTL      int           // session TTL in seconds ; default 10
	Identity string        // value written to the leader key (defaults to host UUID)
	Logger   *slog.Logger
}

// NewElection creates a fresh election session bound to a new
// concurrency Session. The session is owned by the Election — call
// Close() to release it (or rely on ctx cancellation for keepalive
// shutdown). Identity defaults to an empty string ; pass the host
// UUID so the leader's identity is human-readable in etcd.
func NewElection(ctx context.Context, cli *clientv3.Client, opts ElectionOptions) (*Election, error) {
	if cli == nil {
		return nil, fmt.Errorf("etcdcoord: nil etcd client")
	}
	if opts.Key == "" {
		return nil, fmt.Errorf("etcdcoord: election key is required")
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 10
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardW{}, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	sess, err := concurrency.NewSession(cli, concurrency.WithTTL(ttl), concurrency.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("etcdcoord: new session: %w", err)
	}
	el := &Election{
		session:  sess,
		election: concurrency.NewElection(sess, opts.Key),
		owned:    true,
		log:      log,
		key:      opts.Key,
	}
	return el, nil
}

// Campaign blocks until the caller becomes leader OR ctx is cancelled.
// Returns nil on victory, ctx.Err() on cancellation.
func (e *Election) Campaign(ctx context.Context, identity string) error {
	if err := e.election.Campaign(ctx, identity); err != nil {
		return fmt.Errorf("etcdcoord: campaign: %w", err)
	}
	e.log.Info("etcdcoord: became leader", "key", e.key, "identity", identity)
	return nil
}

// TryCampaign tries to become leader without blocking. Returns
// (true, nil) on victory ; (false, nil) when another agent already
// holds the leadership ; (false, err) on transport errors.
func (e *Election) TryCampaign(ctx context.Context, identity string) (bool, error) {
	resp, err := e.election.Leader(ctx)
	if err == nil && resp != nil && len(resp.Kvs) > 0 {
		// Someone already holds it ; only race if THAT leader is us.
		if string(resp.Kvs[0].Value) == identity && resp.Kvs[0].Lease == int64(e.session.Lease()) {
			return true, nil
		}
		return false, nil
	}
	if err := e.election.Campaign(ctx, identity); err != nil {
		return false, fmt.Errorf("etcdcoord: try-campaign: %w", err)
	}
	return true, nil
}

// Resign relinquishes leadership without closing the session, so a
// follower watching Observe() picks up the change. Returns nil if we
// weren't leader to begin with.
func (e *Election) Resign(ctx context.Context) error {
	if err := e.election.Resign(ctx); err != nil {
		return fmt.Errorf("etcdcoord: resign: %w", err)
	}
	return nil
}

// Observe returns a channel that fires the current leader on every
// transition. Useful for followers to learn who's leading without
// campaigning themselves.
func (e *Election) Observe(ctx context.Context) <-chan string {
	out := make(chan string, 4)
	go func() {
		defer close(out)
		ch := e.election.Observe(ctx)
		for resp := range ch {
			if len(resp.Kvs) > 0 {
				select {
				case out <- string(resp.Kvs[0].Value):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// Close releases the underlying session + lease. After Close(), the
// election is unusable. Idempotent.
func (e *Election) Close() error {
	if !e.owned || e.session == nil {
		return nil
	}
	e.owned = false
	return e.session.Close()
}

// ---- helpers -------------------------------------------------------

type discardW struct{}

func (discardW) Write(b []byte) (int, error) { return len(b), nil }

// nowNanos is a seam so tests can inject a fixed clock if needed.
// Default to time.Now().
var nowNanos = func() int64 { return time.Now().UnixNano() }
