package main

// respawn_subscriber.go wires agentrespawn.Subscriber into the daemon :
// subscribes to vm.state_changed + schedulingrule.* events on the
// existing platform event bus and drives the respawn state machine.
//
// VM actions are routed through the Adapter (local host case) ; the
// V0.1 surface respawns VMs by name on the agent that received the
// down signal. A dispatched-respawn path (rule says "VM X on host Y",
// the rule's home agent sends a remote StartVM) is V0.1.1 work, same
// dependency as label-selector matching — both need a clearer
// host-binding shape on SchedulingRuleEntry.
//
// Kept in its own file so the call-site in main.go is one defer and
// the wiring is easy to disable while we land follow-ups.

import (
	"context"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft/agentrespawn"
	"github.com/openweft/weft/etcdcoord"
)

// startRespawnSubscriber starts the bus subscriber + reconciler loop.
// Returns a cancel that stops the goroutine and tears down the bus
// subscription. Always returns a non-nil cancel ; an init failure
// logs + returns a no-op so the daemon shutdown path stays simple.
func startRespawnSubscriber(adp weft.VZAdapter, bus weft.EventBus, etcdCli *clientv3.Client, logger *log.Logger) func() {
	actions := &respawnActions{adp: adp}
	// Pass slog.Default() (NATS-fan-out + stderr per weft-slognats) so
	// the failover plan / claim / election lines land in the same
	// stream as the rest of the agent's structured logs. nil here
	// would drop them all into a discard handler.
	sub := agentrespawn.
		New(bus, respawnRules{adp: adp}, actions, slog.Default()).
		WithStatusReader(respawnStatus{adp: adp})

	ctx, cancel := context.WithCancel(context.Background())

	// V0.1.2 : wire cross-host failover when an etcd client is
	// available. The HostLiveness lease registered at agent boot
	// signals other agents that this host is alive ; the
	// HostWatcher inside the Subscriber reacts to other hosts'
	// lease expiries to claim orphan VMs.
	var hostLiveness *etcdcoord.HostLiveness
	cancelAll := func() {
		cancel()
		// Revoke the long-lived election pool sessions first so any
		// leader keys we hold drop immediately ; downstream pools
		// continue holding their leases otherwise until TTL expiry.
		_ = sub.Close()
		if hostLiveness != nil {
			// Use a fresh ctx — the parent is already cancelled.
			revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer revokeCancel()
			_ = hostLiveness.Stop(revokeCtx)
		}
	}

	if etcdCli != nil {
		localUUID := localHostUUID(adp)
		if localUUID != "" {
			hostname, _ := os.Hostname()
			hl, err := etcdcoord.RegisterHostLiveness(ctx, etcdCli, etcdcoord.HostMetadata{
				HostUUID: localUUID, Hostname: hostname, Hypervisor: os.Getenv("WEFT_HYPERVISOR"),
			}, etcdcoord.LivenessOptions{Logger: slog.Default()})
			if err != nil {
				logger.Printf("respawn subscriber: host liveness register failed (continuing without failover): %v", err)
			} else {
				hostLiveness = hl
				watcher, werr := etcdcoord.NewHostWatcher(ctx, etcdCli, etcdcoord.WatcherOptions{
					IncludeSelf: localUUID, Logger: slog.Default(),
				})
				if werr != nil {
					logger.Printf("respawn subscriber: host watcher init failed (continuing without failover): %v", werr)
				} else {
					sub.WithCoordinator(respawnCoord{adp: adp}, watcher.Events(), etcdCli, "")
					logger.Printf("respawn subscriber: cross-host failover active (host=%s)", localUUID)
					// Periodic reconciliation : any host registered as
					// Active but whose etcd liveness lease is absent +
					// LastSeenAt aged past the takeover threshold must've
					// died while no agent was watching. The runtime
					// watcher catches expiries during this agent's
					// lifetime ; the ticker covers the snapshot case +
					// long-lived phantom entries that never had an
					// agent at all (cluster.hcl-time stubs, killed
					// agents without graceful revoke). After flipping
					// Active→Down, hosts that stay Down past
					// phantomHostDeleteAge are deleted outright so
					// `weft host ls` doesn't accumulate cruft.
					go reconcileStaleHostsLoop(ctx, etcdCli, adp, logger)
				}
			}
		}
	}

	go func() {
		if err := sub.Run(ctx); err != nil && err != context.Canceled {
			logger.Printf("respawn subscriber exited: %v", err)
		}
	}()
	logger.Printf("respawn subscriber: bus subscribed + 2s poll fallback for microVM death")
	return cancelAll
}

// respawnActions adapts Adapter.StartVM/StopVM onto the
// respawn.VMActions interface. cloudInitISO is empty on respawn —
// the VM was already provisioned and its cidata.iso (if any) lives in
// the vmDir from the original create.
type respawnActions struct{ adp weft.VZAdapter }

func (a *respawnActions) StartVM(_ context.Context, name string) error {
	return a.adp.StartVM(name, "")
}
func (a *respawnActions) StopVM(_ context.Context, name string) error {
	return a.adp.StopVM(name)
}

// respawnRules is the SchedulingRulesReader projection of the
// adapter. Kept here (not in agentrespawn) for the same reason as
// watcherScope in floating_ip_nat.go : the dep direction stays
// agentrespawn → weft, not the other way.
type respawnRules struct{ adp weft.VZAdapter }

func (r respawnRules) SchedulingRules() []weft.SchedulingRuleEntry {
	return r.adp.SchedulingRules()
}

// respawnCoord is the HostCoordinator projection : lets the
// Subscriber claim VMs whose inventory record points at a host
// whose etcd-coord lease just expired. Wraps adapter.MigrateVM
// + adapter.ListVMsForHost ; the bus event vm.ownership_claimed
// is already published by MigrateVM (Kind=vm.migrated with
// old_host / new_host meta), so we don't double-publish here.
type respawnCoord struct{ adp weft.VZAdapter }

func (c respawnCoord) LocalHostUUID() string { return localHostUUID(c.adp) }

func (c respawnCoord) VMsOnHost(hostUUID string) []agentrespawn.VMRef {
	vms := c.adp.ListVMsForHost(hostUUID)
	out := make([]agentrespawn.VMRef, 0, len(vms))
	for _, v := range vms {
		out = append(out, agentrespawn.VMRef{
			UUID: v.UUID, Name: v.Name, Project: v.ProjectUUID,
			Properties: v.Properties,
		})
	}
	return out
}

// ListAllVMs returns every VM the registry knows about, across
// projects + hosts. V0.1.8 selector grammar (property-based) consumes
// this on every rescan to find matching VMs without pre-knowing
// their names.
func (c respawnCoord) ListAllVMs() []agentrespawn.VMRef {
	vms := c.adp.VMs()
	out := make([]agentrespawn.VMRef, 0, len(vms))
	for _, v := range vms {
		out = append(out, agentrespawn.VMRef{
			UUID: v.UUID, Name: v.Name, Project: v.ProjectUUID,
			Properties: v.Properties,
		})
	}
	return out
}

func (c respawnCoord) ClaimVM(uuid string) error {
	return c.adp.MigrateVM(uuid, localHostUUID(c.adp))
}

// MarkHostDown flips the host registry's State to Down. Called from
// agentrespawn.consumeHostEvents the moment the etcd watcher observes
// the host's liveness lease expire — without this, idle hosts whose
// dispatch session never opened stayed forever "active" in `weft host
// ls`.  SetHostState is idempotent + tolerant of unknown UUIDs.
func (c respawnCoord) MarkHostDown(uuid string) error {
	return c.adp.SetHostState(uuid, weft.HostStateDown)
}

// respawnStatus is the VMStatusReader projection : tells the
// subscriber whether a microVM is currently alive. We mirror the
// adapter's StatusVM probe logic (exit.json takes precedence over
// the pid liveness check) so the poller sees the same truth a
// `weft microvm ls` call would surface.
type respawnStatus struct{ adp weft.VZAdapter }

// IsVMRunning reads the vmDir for the named VM and returns true iff
// the qemu/vz reaper has NOT written exit.json AND the recorded
// vm.pid maps to a non-zombie process. Mirrors adapter.go's status
// probe in the StatusVM RPC, plus a /proc/<pid>/status zombie check
// the original probe lacks : a SIGKILL'd qemu whose parent driver
// hasn't yet reaped it sits in state 'Z' (defunct), and signal-0
// against a zombie returns nil because the kernel still has the
// PID entry. Without the State check we'd report "running" for the
// duration of the unreaped zombie window — exactly when the respawn
// reconciler wants to see "stopped".
func (r respawnStatus) IsVMRunning(name string) bool {
	vmDir := r.adp.VMDir(name)
	if vmDir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(vmDir, "exit.json")); err == nil {
		return false
	}
	pidBytes, err := os.ReadFile(filepath.Join(vmDir, "vm.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if proc.Signal(syscall.Signal(0)) != nil {
		return false
	}
	// Zombie check via /proc/<pid>/status. Format line "State:\t<X>
	// (...)". On a non-zombie host process X is one of R/S/D/T/t/I
	// (running, sleeping, etc.). 'Z' means defunct — the process is
	// dead but its exit code hasn't been reaped by the parent yet.
	// Treating Z as "stopped" is what `weft microvm ls` does too via
	// the exit.json path ; we just race ahead of the reaper here.
	return !isZombie(pid)
}

// reconcileStaleHosts is the startup-once pass that diffs the host
// registry against the etcd liveness prefix and marks any Active host
// without a live lease as Down. Closes the gap where a host died
// while no agent was watching the cluster (cold cluster boot, dev
// teardown, host crashed before this agent came up) — without it
// those entries stay Active in `weft host ls` forever, misleading the
// operator and the scheduler.
//
// The runtime watcher (consumeHostEvents) already covers expiries
// observed during this agent's lifetime ; this function only handles
// the snapshot-at-boot case. Idempotent : if no hosts are stale, no
// state mutates ; if the etcd Get fails, we log + skip (best-effort).
func reconcileStaleHosts(ctx context.Context, cli *clientv3.Client, adp weft.VZAdapter, logger *log.Logger) {
	// Brief settle window so peer agents finishing their own boot
	// have a chance to publish their liveness leases before we read
	// the prefix — otherwise a cluster-wide restart races itself
	// and we'd false-positive-flip recently-rebooting hosts.
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}

	// Short read window — this is a best-effort snapshot, not a
	// correctness gate. We don't want to block agent startup on a
	// slow etcd quorum, and the runtime watcher catches misses anyway.
	getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := cli.Get(getCtx, etcdcoord.HostsPrefix, clientv3.WithPrefix(), clientv3.WithKeysOnly(), clientv3.WithSerializable())
	if err != nil {
		logger.Printf("reconcile stale hosts: etcd get failed (skipping): %v", err)
		return
	}
	live := make(map[string]struct{}, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		// Key shape is "/weft/coord/hosts/<UUID>" — strip the prefix.
		key := string(kv.Key)
		if !strings.HasPrefix(key, etcdcoord.HostsPrefix) {
			continue
		}
		live[strings.TrimPrefix(key, etcdcoord.HostsPrefix)] = struct{}{}
	}

	now := time.Now()
	var flipped, kept, deleted int
	for _, h := range adp.Hosts() {
		// Down hosts past phantomHostDeleteAge with NO live lease
		// get deleted outright — they're either ex-hosts that never
		// got the operator's explicit delete, or planning stubs
		// (cluster.hcl bring-up registered them, agent never
		// started). Draining stays around indefinitely (operator
		// intent).
		if h.State == weft.HostStateDown {
			if _, alive := live[h.UUID]; alive {
				continue
			}
			if now.Sub(h.LastSeenAt) < phantomHostDeleteAge {
				continue
			}
			if err := adp.DeleteHost(h.UUID); err != nil {
				logger.Printf("reconcile stale hosts: DeleteHost(%s) failed: %v", h.UUID, err)
				continue
			}
			deleted++
			continue
		}
		if h.State != weft.HostStateActive {
			continue // Draining stays as-is
		}
		if _, alive := live[h.UUID]; alive {
			continue
		}
		// No live lease. Use a generous threshold (5 min) so a host
		// that's heartbeating without a lease (e.g. dispatch-only
		// path) isn't wrongly demoted. The takeover policy uses the
		// same threshold via staleHostTakeoverAge.
		if now.Sub(h.LastSeenAt) < 5*time.Minute {
			kept++
			continue
		}
		if err := adp.SetHostState(h.UUID, weft.HostStateDown); err != nil {
			logger.Printf("reconcile stale hosts: SetHostState(%s, down) failed: %v", h.UUID, err)
			continue
		}
		flipped++
	}
	if flipped > 0 || kept > 0 || deleted > 0 {
		logger.Printf("reconcile stale hosts: flipped %d → Down, deleted %d phantom, kept %d Active (recent heartbeat)", flipped, deleted, kept)
	}
}

// phantomHostDeleteAge is the grace period a host stays Down before
// the reconciler deletes it from the registry. The two-stage path
// (Active → Down → deleted) gives operators a window to spot the
// transition and intervene (uncordon, fix the agent) ; sitting Down
// for an hour with no liveness lease + no recent heartbeat is the
// signal that the host genuinely won't come back.
//
// Tuned conservatively — phantom hosts polluting `weft host ls`
// look harmless ; deleting a real-but-quiet host triggers re-
// registration on its next agent boot, which is recoverable but
// noisy. An hour beats both failure modes.
var phantomHostDeleteAge = func() time.Duration {
	if env := os.Getenv("WEFT_PHANTOM_HOST_DELETE_AGE"); env != "" {
		if d, err := time.ParseDuration(env); err == nil {
			return d
		}
	}
	return time.Hour
}()

// reconcileStaleHostsLoop runs reconcileStaleHosts once at startup
// (with a 10s settle window for peer agents to publish their leases)
// and then on a 60s ticker until ctx is cancelled. The ticker covers
// the lifecycle gap : phantom hosts that NEVER had an agent (planning
// stubs) only show up after the snapshot-at-boot pass ; the periodic
// re-check picks them up + drives the Down → delete transition once
// they cross phantomHostDeleteAge.
func reconcileStaleHostsLoop(ctx context.Context, cli *clientv3.Client, adp weft.VZAdapter, logger *log.Logger) {
	reconcileStaleHosts(ctx, cli, adp, logger)
	t := time.NewTicker(reconcileStaleHostsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcileStaleHosts(ctx, cli, adp, logger)
		}
	}
}

// reconcileStaleHostsInterval is how often the loop re-checks the
// host registry against etcd liveness. 60s balances responsiveness
// (phantoms get GC'd within an hour ± 1m) and read load on etcd
// (one Get per minute per agent, KeysOnly + Serializable so it's
// cheap). Tunable via env for tests.
var reconcileStaleHostsInterval = func() time.Duration {
	if env := os.Getenv("WEFT_RECONCILE_HOSTS_INTERVAL"); env != "" {
		if d, err := time.ParseDuration(env); err == nil {
			return d
		}
	}
	return time.Minute
}()

// isZombie returns true when /proc/<pid>/status reports State Z.
// Linux-only ; safe no-op (returns false) when /proc is unavailable
// because the dev/test host isn't Linux.
func isZombie(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "State:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				return f[1] == "Z"
			}
		}
	}
	return false
}
