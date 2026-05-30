package weft

// scheduler.go is the multi-host placement primitive: given a
// VM-creation request + the cluster's Host registry, pick which
// host will run the VM.
//
// Today the only caller is the (future) CloneVM-with-scheduling
// flow; the in-process Bundle path still bypasses it for
// single-host dev. As soon as `weft-agent` lands and there are
// multiple compute hosts, every workload-placing flow (CloneVM,
// RegisterMicroVM, future VM-inventory CreateVM) gets a
// `host_uuid` field that the scheduler fills in.
//
// Design choices:
//
//   * Pure logic, no I/O. Scheduling is a function of the request
//     + the host inventory the caller provides. This means the
//     scheduler is trivially testable + reusable across the
//     control plane.
//
//   * `Scheduler` is an interface so future policies (load-aware,
//     spread-across-AZs, bin-packing) can drop in without churn.
//     The default `FirstFitScheduler` is deliberately dumb —
//     deterministic and stateless — which makes it the right
//     starting point: load-aware policies need metrics we don't
//     collect yet, and bin-packing needs a usage model
//     (over-subscription? hard limits?) we haven't decided.
//
//   * No "soft" preferences yet. Every constraint is hard: if
//     the request asks for the `mesh` network type, hosts that
//     don't list it are dropped entirely. Soft preferences
//     (e.g. "prefer hosts with the `gpu=h100` label") come when
//     we have a real workload that wants them.

import (
	"context"
	"fmt"
)

// ScheduleRequest captures what a VM needs from its host. Every
// non-empty field is a hard constraint; empty fields are
// "no preference".
type ScheduleRequest struct {
	// ProjectUUID + VMName are not used by the scheduler today
	// — they're carried for telemetry / future affinity rules
	// ("place near other VMs of the same project").
	ProjectUUID string
	VMName      string
	// Architecture: "arm64", "amd64", "riscv64", "loongarch64".
	// Must exactly match the host's reported Architecture.
	Architecture string
	// Hypervisor: "apple-vz", "qemu-kvm", "cloud-hypervisor".
	// Must exactly match the host's reported Hypervisor.
	Hypervisor string
	// NetworkTypes: every type listed must be present in the
	// host's NetworkTypes capability list.
	NetworkTypes []string
	// VolumeBackends: every backend listed must be present in
	// the host's VolumeBackends capability list. Host-local
	// drivers like "file" pin the VM to one host; cluster-wide
	// drivers like "ceph" don't.
	VolumeBackends []string
	// AZ: when set, only hosts in this AZ are considered. Empty
	// = any AZ.
	AZ string
	// LabelSelectors: every (key, value) must match a label on
	// the host. Implements hard label matching only — set
	// arithmetic / "in / not-in" come later if needed.
	LabelSelectors map[string]string
}

// Proximity is the cross-replica affinity setting in a
// PlacementRule. The zero value (`ProximityAny`) means "no
// constraint" — the scheduler treats AZ / host as free variables.
//
// Same vs Different express the two HA shapes the user directive
// (2026-05-23) called out :
//
//   - ProximityDifferent: anti-affinity. Each replica goes to a
//     *distinct* AZ (or host). This is how a 3-replica etcd /
//     nats cluster survives a DC outage.
//   - ProximitySame: affinity. All replicas colocate on one AZ
//     (or host). Useful for low-latency clusters, ad-hoc testing,
//     and dev where there's only one AZ anyway.
type Proximity string

const (
	ProximityAny       Proximity = ""
	ProximitySame      Proximity = "same"
	ProximityDifferent Proximity = "different"
)

// PlacementRule expresses how a multi-replica deployment should
// be distributed across the cluster. Per the user directives
// (2026-05-23): "prend en compte le nombre et la proximité: même
// AZ ou AZ differente, meme hyper ou hyper different" + "ajoute
// la notion de rack pour le placement dans une AZ".
//
// Three independent proximity dimensions, evaluated in
// AZ → Rack → Host hierarchy order. A 3-replica plan can ask for
// "one per AZ" (AZ=different) or, when only one AZ is available,
// "one per rack inside the AZ" (AZ=same, Rack=different,
// Host=different) — typical intra-DC HA.
//
// Empty fields mean "no constraint" (`ProximityAny`).
type PlacementRule struct {
	AZ   Proximity
	Rack Proximity
	Host Proximity
}

// GroupScheduleRequest is the multi-replica counterpart of
// ScheduleRequest. Per-replica hard constraints (architecture,
// hypervisor kind, network/volume capabilities, label selectors,
// AZ pin if specified) come from the embedded ScheduleRequest ;
// the Placement field describes the *relative* distribution
// across the replicas in the group.
type GroupScheduleRequest struct {
	ScheduleRequest
	Replicas  int
	Placement PlacementRule
}

// Scheduler picks one or more Hosts from `candidates` for the
// given request(s). Implementations may be stateful (round-robin,
// load-aware) or stateless (first-fit, random) ; the caller
// passes the full host inventory each time so even stateful
// policies don't need their own cache.
//
// Schedule places one VM. ScheduleGroup places `req.Replicas`
// VMs honouring the cross-replica PlacementRule — used by the
// (future) replica-fan-out deployer for HA infra services.
type Scheduler interface {
	Schedule(ctx context.Context, req ScheduleRequest, candidates []Host) (Host, error)
	ScheduleGroup(ctx context.Context, req GroupScheduleRequest, candidates []Host) ([]Host, error)
}

// FirstFitScheduler is the default policy: walk the candidate
// list in order and return the first Active host that matches
// every constraint. Deterministic because Hosts() returns its
// list sorted by (AZ, Hostname) — useful for tests and for
// operators reading audit logs.
//
// Stateless: no per-host load tracking. When multiple hosts
// match, the first one always wins, which means real
// production deployments will want a load-aware policy on top.
// FirstFitScheduler is fine for dev, CI, and single-host
// installs.
type FirstFitScheduler struct{}

// Schedule returns the first matching active host, or an error
// listing the constraints that couldn't be satisfied.
func (FirstFitScheduler) Schedule(ctx context.Context, req ScheduleRequest, candidates []Host) (Host, error) {
	if len(candidates) == 0 {
		return Host{}, fmt.Errorf("schedule: no hosts in the cluster")
	}
	var anyActive bool
	for _, h := range candidates {
		if h.State == HostStateActive {
			anyActive = true
		}
		if !hostMatches(req, h) {
			continue
		}
		return h, nil
	}
	if !anyActive {
		return Host{}, fmt.Errorf("schedule: no active hosts in the cluster (all draining / down)")
	}
	return Host{}, fmt.Errorf("schedule: no active host matches request (project=%s vm=%s arch=%q hyp=%q az=%q net=%v vol=%v labels=%v)",
		req.ProjectUUID, req.VMName, req.Architecture, req.Hypervisor, req.AZ,
		req.NetworkTypes, req.VolumeBackends, req.LabelSelectors)
}

// hostMatches reports whether a single host satisfies every hard
// constraint in the request. State == Active is required —
// Draining hosts don't accept new placements; Down hosts can't
// serve them.
func hostMatches(req ScheduleRequest, h Host) bool {
	if h.State != HostStateActive {
		return false
	}
	// Architecture + Hypervisor matching consults the multi-driver
	// capability list first (a host running both VZ and QEMU declares
	// both via Drivers) ; falls back to the legacy singletons when
	// Drivers is empty.
	if len(h.Drivers) > 0 {
		// Multi-driver host : require the request specify at least
		// the architecture, and find a driver entry covering it. The
		// Hypervisor field, if set, must match the chosen driver's
		// kind.
		if req.Architecture != "" {
			matched := false
			for _, d := range h.Drivers {
				if req.Hypervisor != "" && d.Kind != req.Hypervisor {
					continue
				}
				for _, a := range d.Arches {
					if a == req.Architecture {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				return false
			}
		} else if req.Hypervisor != "" {
			matched := false
			for _, d := range h.Drivers {
				if d.Kind == req.Hypervisor {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	} else {
		if req.Architecture != "" && req.Architecture != h.Architecture {
			return false
		}
		if req.Hypervisor != "" && req.Hypervisor != h.Hypervisor {
			return false
		}
	}
	if req.AZ != "" && req.AZ != h.AZ {
		return false
	}
	for _, want := range req.NetworkTypes {
		if !sliceContains(h.NetworkTypes, want) {
			return false
		}
	}
	for _, want := range req.VolumeBackends {
		if !sliceContains(h.VolumeBackends, want) {
			return false
		}
	}
	for k, want := range req.LabelSelectors {
		if h.Labels[k] != want {
			return false
		}
	}
	return true
}

func sliceContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ScheduleGroup places `req.Replicas` VMs honouring the
// PlacementRule. Greedy: walks `candidates` once per replica and
// picks the first active host that satisfies both the per-replica
// hostMatches() and the cross-replica proximity constraints
// relative to the hosts already chosen for this group.
//
// Output order matches replica index 0..N-1 — callers use it to
// assign per-replica static IPs, DC labels, peer lists, etc.
//
// Errors :
//
//   - Replicas < 1 — operator probably meant 1 ; we surface it
//     loudly instead of guessing.
//   - "no active host matches" for replica i — names i in the
//     error so the operator sees which step ran out of candidates
//     (e.g. "3 AZs requested, only 2 distinct AZs available").
func (FirstFitScheduler) ScheduleGroup(ctx context.Context, req GroupScheduleRequest, candidates []Host) ([]Host, error) {
	if req.Replicas < 1 {
		return nil, fmt.Errorf("schedule-group: replicas must be >= 1, got %d", req.Replicas)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("schedule-group: no hosts in the cluster")
	}
	picked := make([]Host, 0, req.Replicas)
	for i := 0; i < req.Replicas; i++ {
		h, ok := pickNextReplica(req, candidates, picked)
		if !ok {
			return nil, fmt.Errorf("schedule-group: no active host matches replica %d/%d (placement=%+v already-picked=%d arch=%q hyp=%q az=%q)",
				i+1, req.Replicas, req.Placement, len(picked),
				req.Architecture, req.Hypervisor, req.AZ)
		}
		picked = append(picked, h)
	}
	return picked, nil
}

// pickNextReplica scans candidates for the first one that
// satisfies the per-replica request AND the placement rule
// relative to `picked`. Returns (Host, true) on success ;
// (zero, false) when nothing matches.
func pickNextReplica(req GroupScheduleRequest, candidates []Host, picked []Host) (Host, bool) {
	for _, h := range candidates {
		if !hostMatches(req.ScheduleRequest, h) {
			continue
		}
		if !proximityOK(req.Placement, picked, h) {
			continue
		}
		return h, true
	}
	return Host{}, false
}

// proximityOK reports whether `h` is a valid placement for the
// next replica given the already-`picked` hosts and the rule.
//
// AZ / Rack / Host proximity are evaluated independently :
//
//   - ProximityDifferent: the field on `h` must not appear in
//     any already-picked host.
//   - ProximitySame: the field on `h` must equal the first
//     picked host's value. No constraint when picked is empty
//     (the first replica anchors the group).
//   - ProximityAny: no constraint.
//
// Rack-proximity is a no-op when the host's Rack is unset on
// either side — a single-rack cluster genuinely can't satisfy
// "different rack". The caller is expected to set Rack on every
// host in a multi-rack deployment ; missing Rack on one host
// means "we don't know where it is", so we play it safe and
// don't pretend the constraint holds.
func proximityOK(rule PlacementRule, picked []Host, h Host) bool {
	if len(picked) == 0 {
		return true
	}
	if !proximityDimOK(rule.AZ, picked, h, func(x Host) string { return x.AZ }) {
		return false
	}
	if !proximityDimOK(rule.Rack, picked, h, func(x Host) string { return x.Rack }) {
		return false
	}
	if !proximityDimOK(rule.Host, picked, h, func(x Host) string { return x.UUID }) {
		return false
	}
	return true
}

// proximityDimOK is the per-dimension check pulled out so AZ /
// Rack / Host all share the same Same/Different/Any semantics.
// `key` extracts the dimension value from a Host.
func proximityDimOK(p Proximity, picked []Host, h Host, key func(Host) string) bool {
	switch p {
	case ProximityDifferent:
		hv := key(h)
		if hv == "" {
			// Missing dimension on the new host — can't prove
			// "different", fail safe and reject.
			return false
		}
		for _, q := range picked {
			if key(q) == hv {
				return false
			}
		}
	case ProximitySame:
		want := key(picked[0])
		if want == "" {
			// Anchor host had no value — every subsequent host
			// must also have none (genuinely "same unknown")
			// rather than us silently accepting any value.
			return key(h) == ""
		}
		if key(h) != want {
			return false
		}
	}
	return true
}

// ScheduleVM is the Adapter-level entry point: walks the
// cluster's Host registry, hands it to the configured Scheduler,
// returns the chosen Host. The Adapter wires its own
// `a.scheduler` field (defaults to FirstFitScheduler) so
// operators can swap policies without touching call sites.
func (a *Adapter) ScheduleVM(ctx context.Context, req ScheduleRequest) (Host, error) {
	if a.scheduler == nil {
		// Defensive: should never happen post-NewWithStorage but
		// the field has a zero value before the constructor runs.
		a.scheduler = FirstFitScheduler{}
	}
	return a.scheduler.Schedule(ctx, req, a.Hosts())
}

// ScheduleVMGroup is the multi-replica Adapter-level entry point :
// hands `req` + the cluster's Host registry to the configured
// Scheduler, returns one Host per replica in `req.Replicas` order.
// Used by the infra orchestrator's `deployReplica` loop to pick a
// host per replica honouring the plan's PlacementRule.
func (a *Adapter) ScheduleVMGroup(ctx context.Context, req GroupScheduleRequest) ([]Host, error) {
	if a.scheduler == nil {
		a.scheduler = FirstFitScheduler{}
	}
	return a.scheduler.ScheduleGroup(ctx, req, a.Hosts())
}

// SetScheduler swaps the placement policy. Used by tests and by
// `weft.hcl`'s future `scheduler { policy = "..." }` block.
func (a *Adapter) SetScheduler(s Scheduler) {
	if s == nil {
		s = FirstFitScheduler{}
	}
	a.scheduler = s
}
