// Package zombiegc periodically reconciles the VM registry against
// actual VM liveness + host availability, identifies zombie records
// (VMs the registry thinks are running but aren't), and applies a
// policy matrix :
//
//	Local zombie         — VM record points at this host, state=running,
//	                       but the local process is gone (agent crashed
//	                       mid-VM, manual kill -9, OOM-kill on qemu).
//	                       Action : mark state=zombie + bus event ;
//	                       operator deletes via `weft instance gc --apply`.
//
//	CI cross-host zombie — deployment.type=ci AND owning host has been
//	                       Down past its etcd lease grace AND respawn
//	                       was skipped by the V0.1.10 CI gate.
//	                       Action : mark state=zombie, then auto-delete
//	                       after the configurable CI grace period.
//	                       Disposable by convention, safe to drop.
//
//	HA cross-host zombie — non-CI VM whose host has been Down past
//	                       lease grace ; either claimed by another agent
//	                       (in which case it's not zombie) or stuck
//	                       (respawn rule misconfigured, no covering rule).
//	                       Action : mark state=zombie + log alert ;
//	                       NEVER auto-delete (would lose data).
//
//	Orphan project       — project_uuid no longer exists in the registry.
//	                       Action : mark state=zombie + alert.
//	                       NEVER auto-delete (likely an admin mistake
//	                       worth investigating).
//
// V0.1.12 — closes the gap left by the V0.1.10 CI gate : skipping
// respawn was correct, but the zombie record accumulated forever in
// etcd until something cleaned it. Now the GC cleans it.
package zombiegc

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	weft "github.com/openweft/weft"
)

// ZombieKind classifies a detected zombie by its cause. Drives the
// per-kind action policy + Prometheus label.
type ZombieKind string

const (
	ZombieLocal         ZombieKind = "local"          // host=me, state=running, process gone
	ZombieCICrossHost   ZombieKind = "ci_cross_host"  // deployment.type=ci, host down
	ZombieHACrossHost   ZombieKind = "ha_cross_host"  // non-CI, host down
	ZombieOrphanProject ZombieKind = "orphan_project" // project_uuid no longer exists
	// ZombieOrphanDir : a vmDir on disk with no matching registry
	// record. Typical cause : agent crashed mid-RegisterMicroVM, or
	// manual rm of the registry record while the disk artifact
	// stayed behind. Detected by ListVMDirs against the in-memory
	// vmReg. NEVER auto-deleted by default — disk artifacts are
	// indistinguishable from "VM whose record is about to be
	// re-registered" without an age signal.
	ZombieOrphanDir ZombieKind = "orphan_dir"
)

// Zombie is one detection. The Reason field copies enough host /
// process state at the moment of detection to let the operator
// understand why the GC flagged this VM without re-running the
// query later (the host could have come back online by the time
// they look).
type Zombie struct {
	UUID            string
	Name            string
	ProjectUUID     string
	HostUUID        string
	Kind            ZombieKind
	Reason          string
	DetectedAt      time.Time
	DeploymentType  string // properties["deployment.type"], for the action policy
	HostDownSince   time.Time
}

// Report is the result of one Sweep. Useful for the CLI to render
// + for the Prometheus gauge to count.
type Report struct {
	Zombies []Zombie
	// Deleted is the count auto-deleted during this sweep ; never
	// includes the dry-run path.
	Deleted int
}

// Options configures a Reconciler. Zero-value is usable but Adapter +
// LocalHostUUID must be supplied through the constructor.
type Options struct {
	// CIGracePeriod is how long a CI VM stays marked state=zombie
	// before auto-delete. Default : 1h. Set via WEFT_ZOMBIE_GC_CI_GRACE.
	CIGracePeriod time.Duration
	// SweepInterval is how often the Reconciler runs Sweep. Default :
	// 5min. Set via WEFT_ZOMBIE_GC_SWEEP_INTERVAL.
	SweepInterval time.Duration
	// HostDownGrace is how long a host must have been unreachable
	// before we consider its VMs zombies. Default : 2 × the etcd
	// lease (≈30s), so 60s — long enough that a slow heartbeat
	// doesn't trigger false positives, short enough that operators
	// see the report on the same shift.
	HostDownGrace time.Duration
	// OrphanDirGrace is how old a vmDir must be (ModTime) before we
	// flag it as an orphan_dir zombie. Default : 5 minutes — short
	// enough that operators see disk leaks the same shift, long
	// enough that a freshly-created VM (RegisterMicroVM mid-flight)
	// doesn't race into the report.
	OrphanDirGrace time.Duration
	// OrphanDirAutoDeleteAfter, when > 0, enables auto-deletion of
	// orphan_dirs whose age is older than this threshold. Default :
	// 0 (disabled — orphan_dirs mark only). Set to 24h-7d for
	// long-term housekeeping ; a vmDir untouched for that long is
	// extremely unlikely to be a VM mid-claim. Set to 0 to keep
	// the original V0.1.13 mark-only behaviour.
	OrphanDirAutoDeleteAfter time.Duration
	// DryRun disables auto-delete + state mutations. The Sweep still
	// returns the Report so callers can render what WOULD happen.
	DryRun bool
	// Logger for progress + audit lines. Defaults to slog.Default().
	Logger *slog.Logger
}

// VMLivenessProbe tells the Reconciler whether a named VM is
// currently running on this host. Production wires this to the same
// poller agentrespawn uses (exit.json + vm.pid check) so both
// subsystems see the same truth.
type VMLivenessProbe interface {
	IsVMRunning(name string) bool
}

// Reconciler is the long-lived GC ; one per host inside weft agent.
type Reconciler struct {
	adp           weft.VZAdapter
	probe         VMLivenessProbe
	localHostUUID string
	opts          Options
	log           *slog.Logger

	mu    sync.Mutex
	last  Report
	stats Stats
}

// Stats are exported gauges. The cmd/weft Prometheus shim polls
// these from a tick goroutine.
type Stats struct {
	ZombiesByKind map[ZombieKind]int
	DeletedTotal  uint64
	LastSweepAt   time.Time
}

// New returns a Reconciler ready to Run. localHostUUID is the host
// UUID this agent identifies as ; needed to distinguish "local"
// zombies from cross-host.
func New(adp weft.VZAdapter, probe VMLivenessProbe, localHostUUID string, opts Options) *Reconciler {
	if opts.CIGracePeriod <= 0 {
		opts.CIGracePeriod = 1 * time.Hour
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = 5 * time.Minute
	}
	if opts.HostDownGrace <= 0 {
		opts.HostDownGrace = 60 * time.Second
	}
	if opts.OrphanDirGrace <= 0 {
		opts.OrphanDirGrace = 5 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Reconciler{
		adp:           adp,
		probe:         probe,
		localHostUUID: localHostUUID,
		opts:          opts,
		log:           opts.Logger,
		stats:         Stats{ZombiesByKind: make(map[ZombieKind]int)},
	}
}

// Run loops on SweepInterval until ctx is cancelled. Use this as a
// goroutine inside weft agent ; for one-shot use call Sweep directly.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.opts.SweepInterval)
	defer t.Stop()
	// Fire one sweep immediately so the first metric tick has data.
	r.runSweepLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runSweepLogged(ctx)
		}
	}
}

func (r *Reconciler) runSweepLogged(ctx context.Context) {
	rep := r.Sweep(ctx)
	if len(rep.Zombies) == 0 && rep.Deleted == 0 {
		return
	}
	names := make([]string, 0, len(rep.Zombies))
	for _, z := range rep.Zombies {
		names = append(names, fmt.Sprintf("%s(%s,dep=%s)", z.Name, z.Kind, z.DeploymentType))
	}
	r.log.Info("zombiegc : sweep complete",
		"zombies_total", len(rep.Zombies),
		"deleted", rep.Deleted,
		"dry_run", r.opts.DryRun,
		"zombies", names,
	)
}

// LastReport returns a copy of the most recent sweep result.
func (r *Reconciler) LastReport() Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Report{Deleted: r.last.Deleted}
	out.Zombies = append(out.Zombies, r.last.Zombies...)
	return out
}

// StatsSnapshot returns a copy of the running stats. Safe for
// concurrent Prometheus polling.
func (r *Reconciler) StatsSnapshot() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Stats{
		DeletedTotal:  r.stats.DeletedTotal,
		LastSweepAt:   r.stats.LastSweepAt,
		ZombiesByKind: make(map[ZombieKind]int, len(r.stats.ZombiesByKind)),
	}
	for k, v := range r.stats.ZombiesByKind {
		out.ZombiesByKind[k] = v
	}
	return out
}

// Sweep walks the VM registry, classifies zombies, and applies the
// policy. Returns the report ; the same report is cached as the
// LastReport.
func (r *Reconciler) Sweep(ctx context.Context) Report {
	now := time.Now().UTC()
	vms := r.adp.VMs()

	// Build a host lookup once : map[uuid]Host. Used to evaluate
	// host-state + lease-age for cross-host classifications.
	hosts := r.adp.Hosts()
	hostByUUID := make(map[string]weft.Host, len(hosts))
	for _, h := range hosts {
		hostByUUID[h.UUID] = h
	}

	// Build a project-existence set so orphan-project classification
	// is O(1) per VM.
	projects := r.adp.Projects()
	projectExists := make(map[string]struct{}, len(projects))
	for _, p := range projects {
		projectExists[p.UUID] = struct{}{}
	}

	var rep Report
	byKind := map[ZombieKind]int{}

	// Build a (projectUUID, name) index from the in-memory registry so
	// the orphan_dir scan is O(1) per disk entry.
	knownDirs := make(map[string]struct{}, len(vms))
	for _, vm := range vms {
		knownDirs[dirKey(vm.ProjectUUID, vm.Name)] = struct{}{}
	}

	for _, vm := range vms {
		z, ok := r.classify(vm, hostByUUID, projectExists, now)
		if !ok {
			continue
		}
		rep.Zombies = append(rep.Zombies, z)
		byKind[z.Kind]++

		// Apply the policy.
		if r.opts.DryRun {
			continue
		}
		// 1. Mark state=zombie if not already.
		if vm.State != weft.VMStateZombie {
			if err := r.adp.SetVMState(vm.UUID, weft.VMStateZombie); err != nil {
				r.log.Warn("zombiegc : mark zombie failed",
					"vm", vm.Name, "uuid", vm.UUID, "err", err)
				continue
			}
		}
		// 2. Auto-delete CI zombies past grace. Covers both
		// ZombieCICrossHost (host went down, respawn was skipped)
		// AND ZombieLocal where deployment.type=ci (the qemu
		// crashed locally, no investigation value for a disposable
		// build runner).
		if (z.Kind == ZombieCICrossHost ||
			(z.Kind == ZombieLocal && vm.Properties["deployment.type"] == "ci")) &&
			r.ciGraceExpired(vm, z, now) {
			if err := r.adp.DeleteVM(vm.Name); err != nil {
				r.log.Warn("zombiegc : delete ci zombie failed",
					"vm", vm.Name, "uuid", vm.UUID, "err", err)
				continue
			}
			r.log.Info("zombiegc : ci zombie auto-deleted",
				"vm", vm.Name, "uuid", vm.UUID,
				"host_down_since", z.HostDownSince)
			rep.Deleted++
		}
	}

	// V0.1.13 : orphan_dir scan. Walks the disk-side vmsDir and
	// flags any directory that doesn't have a corresponding
	// registry record. Catches the "crash mid-RegisterMicroVM" and
	// "manual rm of registry record" cases that the registry-only
	// sweep above misses by definition.
	for _, d := range r.adp.ListVMDirs() {
		if _, known := knownDirs[dirKey(d.ProjectUUID, d.Name)]; known {
			continue
		}
		age := now.Sub(d.ModTime)
		if age < r.opts.OrphanDirGrace {
			// Too young — likely a RegisterMicroVM in flight.
			continue
		}
		z := Zombie{
			Name:        d.Name,
			ProjectUUID: d.ProjectUUID,
			Kind:        ZombieOrphanDir,
			Reason:      fmt.Sprintf("vmDir %q has no matching registry record (age=%s)", d.Path, age.Truncate(time.Second)),
			DetectedAt:  now,
		}
		rep.Zombies = append(rep.Zombies, z)
		byKind[ZombieOrphanDir]++

		// V0.1.14 : optional auto-delete past a long grace.
		// Default 0 = disabled (V0.1.13 mark-only behaviour).
		// When set (typically 24h-7d), reaping disk leaks without
		// operator intervention. Dry-run still respected.
		if r.opts.DryRun {
			continue
		}
		if r.opts.OrphanDirAutoDeleteAfter <= 0 {
			continue
		}
		if age < r.opts.OrphanDirAutoDeleteAfter {
			continue
		}
		if err := r.adp.DeleteVMDir(d.ProjectUUID, d.Name); err != nil {
			r.log.Warn("zombiegc : delete orphan_dir failed",
				"project", d.ProjectUUID, "name", d.Name,
				"path", d.Path, "err", err)
			continue
		}
		r.log.Info("zombiegc : orphan_dir auto-deleted",
			"project", d.ProjectUUID, "name", d.Name,
			"path", d.Path, "age", age.Truncate(time.Second))
		rep.Deleted++
	}

	r.mu.Lock()
	r.last = rep
	r.stats.LastSweepAt = now
	r.stats.ZombiesByKind = byKind
	r.stats.DeletedTotal += uint64(rep.Deleted)
	r.mu.Unlock()
	return rep
}

// dirKey builds the lookup key the orphan_dir scan uses to test
// whether a vmDir matches a registry record. Mirrors the on-disk
// path layout (<projectUUID>/<vmName>) so the comparison is
// straightforward.
func dirKey(projectUUID, name string) string {
	return projectUUID + "\x00" + name
}

// classify decides whether `vm` is a zombie, and if so which kind.
// Returns (Zombie, true) on hit, (_, false) on miss.
func (r *Reconciler) classify(vm weft.VM, hostByUUID map[string]weft.Host, projectExists map[string]struct{}, now time.Time) (Zombie, bool) {
	// Orphan project is the only classification that's independent
	// of state ; mark even non-running records since they shouldn't
	// be in the registry at all.
	if _, ok := projectExists[vm.ProjectUUID]; !ok {
		return Zombie{
			UUID:           vm.UUID,
			Name:           vm.Name,
			ProjectUUID:    vm.ProjectUUID,
			HostUUID:       vm.HostUUID,
			Kind:           ZombieOrphanProject,
			Reason:         fmt.Sprintf("project %q no longer exists in registry", vm.ProjectUUID),
			DetectedAt:     now,
			DeploymentType: vm.Properties["deployment.type"],
		}, true
	}

	// VMs in terminal states aren't zombie candidates. State=zombie
	// is already a zombie and stays one until deletion ; we still
	// surface it on every sweep so the report stays complete.
	//
	// "created" is included as a zombie candidate because the
	// registry only transitions VMs to "running" when the platform
	// receives the StartVM ACK ; an agent crash mid-Start can leave
	// a VM "created" with a stale vm.pid file. The local probe
	// catches both cases identically.
	switch vm.State {
	case weft.VMStateRunning, weft.VMStateCreated:
		// fallthrough into the classification below.
	case weft.VMStateZombie:
		// A previously-classified zombie may have come back to
		// life — typical sequence : agent crashed → zombiegc
		// flagged → operator restarted weft-agent → self-heal
		// restarted qemu → state should reset to Running. Without
		// the live probe here, classifyExistingZombie sticks the
		// "awaiting delete" label forever even when the process is
		// patently alive (operator-reported 2026-06-29 "encore des
		// microvm en state zombie" after a successful self-heal
		// pass). For the local host : if the qemu probe says
		// alive, flip state back to Running + drop the zombie
		// record on this sweep. Cross-host probes aren't available
		// from this side, so they keep the existing semantics.
		if vm.HostUUID == r.localHostUUID && r.probe != nil && r.probe.IsVMRunning(vm.Name) {
			if err := r.adp.SetVMState(vm.UUID, weft.VMStateRunning); err != nil {
				r.log.Warn("zombiegc : un-zombie failed",
					"vm", vm.Name, "uuid", vm.UUID, "err", err)
			} else {
				r.log.Info("zombiegc : un-zombie (qemu alive)",
					"vm", vm.Name, "uuid", vm.UUID)
				return Zombie{}, false
			}
		}
		return r.classifyExistingZombie(vm, hostByUUID, now), true
	default:
		return Zombie{}, false
	}

	host, hostKnown := hostByUUID[vm.HostUUID]

	// Local zombie : the owning host is this agent.
	if vm.HostUUID == r.localHostUUID {
		if r.probe != nil && r.probe.IsVMRunning(vm.Name) {
			return Zombie{}, false
		}
		return Zombie{
			UUID:           vm.UUID,
			Name:           vm.Name,
			ProjectUUID:    vm.ProjectUUID,
			HostUUID:       vm.HostUUID,
			Kind:           ZombieLocal,
			Reason:         "host is local but no process found for VM",
			DetectedAt:     now,
			DeploymentType: vm.Properties["deployment.type"],
		}, true
	}

	// Cross-host classification : need to know host state.
	if !hostKnown {
		return Zombie{
			UUID:           vm.UUID,
			Name:           vm.Name,
			ProjectUUID:    vm.ProjectUUID,
			HostUUID:       vm.HostUUID,
			Kind:           ZombieHACrossHost,
			Reason:         fmt.Sprintf("host %q no longer exists in registry", vm.HostUUID),
			DetectedAt:     now,
			DeploymentType: vm.Properties["deployment.type"],
		}, true
	}
	hostDownFor := now.Sub(host.LastSeenAt)
	if host.State == weft.HostStateActive && hostDownFor < r.opts.HostDownGrace {
		// Host alive + recent heartbeat : VM is healthy.
		return Zombie{}, false
	}
	// Cross-host zombie. Disambiguate CI vs HA.
	kind := ZombieHACrossHost
	if vm.Properties["deployment.type"] == "ci" {
		kind = ZombieCICrossHost
	}
	return Zombie{
		UUID:           vm.UUID,
		Name:           vm.Name,
		ProjectUUID:    vm.ProjectUUID,
		HostUUID:       vm.HostUUID,
		Kind:           kind,
		Reason:         fmt.Sprintf("host %q down for %s (state=%s)", host.UUID, hostDownFor.Truncate(time.Second), host.State),
		DetectedAt:     now,
		DeploymentType: vm.Properties["deployment.type"],
		HostDownSince:  host.LastSeenAt,
	}, true
}

// classifyExistingZombie re-evaluates a VM already marked
// state=zombie. Returns its current classification so the report
// stays complete even between mark + delete sweeps.
func (r *Reconciler) classifyExistingZombie(vm weft.VM, hostByUUID map[string]weft.Host, now time.Time) Zombie {
	host, hostKnown := hostByUUID[vm.HostUUID]
	kind := ZombieHACrossHost
	if vm.Properties["deployment.type"] == "ci" {
		kind = ZombieCICrossHost
	} else if vm.HostUUID == r.localHostUUID {
		kind = ZombieLocal
	}
	var since time.Time
	if hostKnown {
		since = host.LastSeenAt
	}
	return Zombie{
		UUID:           vm.UUID,
		Name:           vm.Name,
		ProjectUUID:    vm.ProjectUUID,
		HostUUID:       vm.HostUUID,
		Kind:           kind,
		Reason:         "VM already marked state=zombie ; awaiting delete (CI) or operator action",
		DetectedAt:     now,
		DeploymentType: vm.Properties["deployment.type"],
		HostDownSince:  since,
	}
}

// ciGraceExpired returns true when a CI zombie has been in this state
// long enough to auto-delete. Reference time differs by zombie kind :
//
//	ZombieCICrossHost : use HostDownSince — the host going down is
//	                    when the VM stopped serving its purpose.
//	ZombieLocal       : use LastStartAt (or CreatedAt if never started)
//	                    — there's no host-down event to anchor on,
//	                    so we measure from when the VM was last
//	                    intended to be alive.
func (r *Reconciler) ciGraceExpired(vm weft.VM, z Zombie, now time.Time) bool {
	switch z.Kind {
	case ZombieCICrossHost:
		if z.HostDownSince.IsZero() {
			ref := vm.LastStartAt
			if ref.IsZero() {
				ref = vm.CreatedAt
			}
			return now.Sub(ref) >= r.opts.CIGracePeriod
		}
		return now.Sub(z.HostDownSince) >= r.opts.CIGracePeriod
	case ZombieLocal:
		ref := vm.LastStartAt
		if ref.IsZero() {
			ref = vm.CreatedAt
		}
		return now.Sub(ref) >= r.opts.CIGracePeriod
	default:
		return false
	}
}
