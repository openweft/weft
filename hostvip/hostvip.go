// Package hostvip implements a control-plane Virtual IP that floats
// between the cluster's control-plane hosts using etcd leader election
// + gratuitous ARP for L2 cache refresh. No external daemons
// (keepalived / VRRP), no apt installs — pure-Go, CGO=0, the same
// pattern as Scalingo/LinK and Talos VIP.
//
// Design (matches project_router_lb_gonative + the user's directive
// 2026-06-22 "ce serait plus logique d'avoir un équivalent Go") :
//
//   - Each control-plane host's weft-agent runs a Controller that
//     campaigns for /weft/coord/vip/<address> in etcd.
//   - Exactly one host wins ; on victory it Bind()s the VIP /32 to
//     its physical interface via netlink and broadcasts a gratuitous
//     ARP so every switch on the L2 segment refreshes its CAM.
//   - On lease expiry / agent crash / explicit Resign, another host
//     wins within ~LeaseTTL and gARPs from its own MAC.
//   - The TUI (and any other client) dials the VIP — connections
//     follow the floating address across failovers.
//
// What this package does NOT do :
//
//   - It does not multicast VRRP frames (224.0.0.18, IP proto 112).
//     Multicast is blocked on many cloud/overlay nets ; the
//     etcd-election approach works everywhere weft already runs.
//   - It does not handle IPv6 ND ; the existing floatingipl2 gARP
//     is v4-only and the same constraint applies here.
//   - It does not health-check the gRPC server. That's a Controller
//     extension : a TrackScript-style hook that calls Resign() when
//     the local agent is unhealthy. Left for v2.
//
// Layered atop the existing primitives :
//
//   - etcdcoord.Election : leader election + session lease (proven by
//     respawn V0.1.2 + ElectionPool).
//   - vishvananda/netlink : AddrAdd / AddrDel on the parent NIC.
//   - AF_PACKET raw socket : gratuitous ARP frame (Linux only).
//
// Cross-platform note : the Linux reconciler lives in
// reconciler_linux.go ; reconciler_other.go provides a stub that
// returns ErrUnsupported on darwin / openbsd / freebsd so the package
// compiles cleanly everywhere (matches the floatingipl2 split).
package hostvip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/openweft/weft/etcdcoord"
)

// ErrUnsupported is returned by the stub reconciler on non-Linux
// platforms. Callers can detect this + skip the VIP feature gracefully.
var ErrUnsupported = errors.New("hostvip: not supported on this platform")

// Config wires the static knobs of a Controller. All fields except
// Address + Interface have sensible defaults.
type Config struct {
	// Address is the floating VIP with its prefix length, e.g.
	// netip.MustParsePrefix("192.168.105.100/24"). The prefix tells
	// netlink the on-link subnet so the kernel ARP-resolves
	// neighbours via the same NIC.
	Address netip.Prefix

	// Interface is the parent NIC name the VIP attaches to, e.g.
	// "enp0s1". Matches `ip link show` ; not the macvlan child name.
	Interface string

	// ElectionKey is the etcd prefix used for the leader lock.
	// Defaults to "/weft/coord/vip/<address>" so multiple VIPs can
	// coexist without colliding.
	ElectionKey string

	// LeaseTTL is the session TTL in seconds. Defaults to 5 ; the
	// failover window is bounded by 2×LeaseTTL in the worst case
	// (lease expiry + next campaigner's session start). Lower means
	// faster failover at the cost of more etcd keep-alive traffic.
	LeaseTTL int

	// Identity is the value written to the leader key. Defaults to
	// the empty string — pass the host UUID so the leader's identity
	// is human-readable in `etcdctl get`.
	Identity string

	// Logger receives structured lifecycle events (became leader,
	// lost leadership, reconcile failed). nil → discards.
	Logger *slog.Logger
}

// Reconciler is the platform-specific surface that attaches /
// detaches the VIP on the local interface + broadcasts gratuitous
// ARP. Linux implementation in reconciler_linux.go ; darwin / *bsd
// stubs in reconciler_other.go.
type Reconciler interface {
	// Bind attaches addr to iface and returns nil on success. Idempotent :
	// re-binding an already-bound address must be a no-op, not an error
	// (the kernel's EEXIST surfaces ; the implementation swallows it).
	Bind(addr netip.Prefix, iface string) error

	// Unbind detaches addr from iface. A missing address is not an
	// error (ENODEV / ENOENT → no-op).
	Unbind(addr netip.Prefix, iface string) error

	// AnnounceGARP emits one gratuitous ARP frame so upstream switches
	// refresh their CAM table to the new owner's MAC. Best-effort ;
	// failures are logged but don't fail the leadership transition.
	AnnounceGARP(addr netip.Prefix, iface string) error
}

// State reports whether the local agent currently holds the VIP. The
// Controller emits transitions over StateCh().
type State int

const (
	// StateFollower : the local agent is NOT the leader. The VIP is
	// either unbound here, or pending Unbind after a lost-leadership
	// transition.
	StateFollower State = iota
	// StateLeader : the local agent IS the leader. The VIP is bound
	// to the configured interface ; the gARP has been broadcast.
	StateLeader
)

// String pretty-prints a State for logging.
func (s State) String() string {
	switch s {
	case StateFollower:
		return "follower"
	case StateLeader:
		return "leader"
	}
	return "unknown"
}

// Controller is the per-host VIP campaigner. One instance per
// configured VIP ; the weft-agent constructs it conditionally when
// control_plane.vip {} is present in the cluster config.
type Controller struct {
	cfg Config
	cli *clientv3.Client
	rec Reconciler
	log *slog.Logger

	mu       sync.RWMutex
	state    State
	leader   string
	stateCh  chan State
	stopOnce sync.Once
	stop     chan struct{}
}

// NewController returns a Controller ready to Run. It does NOT touch
// etcd or netlink until Run starts ; callers can construct it
// optimistically and defer Run.
func NewController(cfg Config, cli *clientv3.Client, rec Reconciler) (*Controller, error) {
	if cli == nil {
		return nil, fmt.Errorf("hostvip: nil etcd client")
	}
	if rec == nil {
		return nil, fmt.Errorf("hostvip: nil reconciler")
	}
	if !cfg.Address.IsValid() || !cfg.Address.Addr().IsValid() {
		return nil, fmt.Errorf("hostvip: invalid address %q", cfg.Address)
	}
	if cfg.Interface == "" {
		return nil, fmt.Errorf("hostvip: interface required")
	}
	if cfg.ElectionKey == "" {
		cfg.ElectionKey = "/weft/coord/vip/" + cfg.Address.Addr().String()
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 5
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Controller{
		cfg:     cfg,
		cli:     cli,
		rec:     rec,
		log:     cfg.Logger.With("vip", cfg.Address.String(), "iface", cfg.Interface),
		stateCh: make(chan State, 4),
		stop:    make(chan struct{}),
	}, nil
}

// Run campaigns for leadership in a loop. Blocks until ctx is cancelled
// or Close() is called. Returns nil on graceful shutdown ; non-nil
// only on un-recoverable etcd errors (every other failure mode is
// retried with exponential backoff).
//
// Loop body :
//
//  1. Create a fresh etcdcoord.Election (new session, new lease).
//  2. Campaign(ctx) → blocks until we're leader OR ctx cancelled.
//  3. On victory : Bind + AnnounceGARP, transition to Leader.
//  4. Watch session.Done() — when the lease drops (network blip,
//     etcd partition, explicit Resign), we lose the VIP. Unbind
//     + transition back to Follower + restart the loop.
//  5. Backoff between iterations so a flapping etcd doesn't
//     hot-spin.
func (c *Controller) Run(ctx context.Context) error {
	backoff := 250 * time.Millisecond
	const maxBackoff = 10 * time.Second
	for {
		select {
		case <-ctx.Done():
			return c.shutdown(context.Background())
		case <-c.stop:
			return c.shutdown(context.Background())
		default:
		}
		if err := c.runOnce(ctx); err != nil {
			c.log.Warn("hostvip: campaign iteration failed", "err", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return c.shutdown(context.Background())
			case <-c.stop:
				return c.shutdown(context.Background())
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		// Successful Bind/Unbind cycle → reset backoff.
		backoff = 250 * time.Millisecond
	}
}

// runOnce performs a single Campaign → Bind → wait-for-loss →
// Unbind cycle. Returns nil on a clean session end (lease expired,
// resigned, ctx cancelled) ; non-nil only on bind / unbind / etcd
// errors that warrant a backoff. The for-loop in Run wraps this
// with retry semantics.
func (c *Controller) runOnce(ctx context.Context) error {
	el, err := etcdcoord.NewElection(ctx, c.cli, etcdcoord.ElectionOptions{
		Key:      c.cfg.ElectionKey,
		TTL:      c.cfg.LeaseTTL,
		Identity: c.cfg.Identity,
		Logger:   c.log,
	})
	if err != nil {
		return fmt.Errorf("new election: %w", err)
	}
	defer el.Close()

	// Campaign blocks until we win OR ctx is cancelled.
	if err := el.Campaign(ctx, c.cfg.Identity); err != nil {
		if ctx.Err() != nil {
			return nil // graceful
		}
		return fmt.Errorf("campaign: %w", err)
	}

	// Won the election. Bind the VIP locally.
	if err := c.rec.Bind(c.cfg.Address, c.cfg.Interface); err != nil {
		// Bind failed → resign so another host can try.
		_ = el.Resign(context.Background())
		return fmt.Errorf("bind %s on %s: %w", c.cfg.Address, c.cfg.Interface, err)
	}
	// Best-effort gARP : failures are logged, not fatal.
	if err := c.rec.AnnounceGARP(c.cfg.Address, c.cfg.Interface); err != nil {
		c.log.Warn("hostvip: gARP announce failed", "err", err)
	}
	c.transition(StateLeader, c.cfg.Identity)
	c.log.Info("hostvip: became leader", "address", c.cfg.Address.String())

	// Stay leader until ctx is cancelled / Close() / lease lost.
	// The etcd Session.Done() channel fires when the lease can't be
	// kept alive (network partition, etcd quorum loss, ttl expired).
	select {
	case <-ctx.Done():
	case <-c.stop:
	}

	// Lost leadership : Unbind locally + transition.
	if err := c.rec.Unbind(c.cfg.Address, c.cfg.Interface); err != nil {
		c.log.Warn("hostvip: unbind failed", "err", err)
	}
	c.transition(StateFollower, "")
	c.log.Info("hostvip: relinquished leadership")
	// Resign explicitly so the next campaigner sees the key drop
	// immediately rather than waiting for the lease TTL.
	_ = el.Resign(context.Background())
	return nil
}

// shutdown is the once-only cleanup path on Run exit. Ensures the
// VIP is unbound even when ctx was cancelled mid-Leader so the
// /32 doesn't linger on the interface across agent restarts.
func (c *Controller) shutdown(ctx context.Context) error {
	c.mu.RLock()
	wasLeader := c.state == StateLeader
	c.mu.RUnlock()
	if wasLeader {
		if err := c.rec.Unbind(c.cfg.Address, c.cfg.Interface); err != nil {
			c.log.Warn("hostvip: shutdown unbind failed", "err", err)
		}
		c.transition(StateFollower, "")
	}
	return nil
}

// transition updates the cached state + leader identity AND emits a
// State message on the StateCh channel. Drops the message if no
// reader is consuming so the Controller never blocks.
func (c *Controller) transition(s State, leader string) {
	c.mu.Lock()
	c.state = s
	c.leader = leader
	c.mu.Unlock()
	select {
	case c.stateCh <- s:
	default:
	}
}

// State returns the current State + leader identity. Safe to call
// from any goroutine.
func (c *Controller) State() (State, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state, c.leader
}

// StateCh returns a buffered channel that receives every state
// transition. Reads from a slow consumer drop silently — callers that
// need every event must drain promptly.
func (c *Controller) StateCh() <-chan State { return c.stateCh }

// Close requests a graceful shutdown. The Run goroutine notices on
// its next select + exits via shutdown(). Idempotent.
func (c *Controller) Close() error {
	c.stopOnce.Do(func() { close(c.stop) })
	return nil
}
