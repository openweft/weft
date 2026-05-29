package infra

// scheduler_bridge.go translates between the operator-friendly
// HCL `placement { … }` block on a Plan and the scheduler's
// runtime types in package `weft` (PlacementRule, Proximity,
// GroupScheduleRequest). Kept in the infra package — not in
// `weft` — so the loader stays independent of the scheduler ;
// the bridge is only used at deploy time.
//
// The conversion is deliberately small and dependency-free :
// neither side references the other beyond these helpers. The
// deployer ([[infra-in-micro-vms]] code in cmd/weft/infra.go)
// is the only caller.

import (
	"fmt"

	weft "github.com/openweft/weft"
)

// PlacementRule renders the plan's placement intent into the
// scheduler's runtime PlacementRule. A nil PlacementBlk yields
// the zero value (every dimension = ProximityAny) — the
// default-deploy-anywhere shape for single-replica plans.
//
// Validate() should already have been run at LoadPlan time, so
// unrecognised proximity strings are a programmer error here ;
// we still guard against them with an explicit error so a stale
// in-memory Plan never reaches the scheduler.
func (b *PlacementBlk) PlacementRule() (weft.PlacementRule, error) {
	if b == nil {
		return weft.PlacementRule{}, nil
	}
	az, err := toProximity(b.AZ)
	if err != nil {
		return weft.PlacementRule{}, fmt.Errorf("placement.az: %w", err)
	}
	rack, err := toProximity(b.Rack)
	if err != nil {
		return weft.PlacementRule{}, fmt.Errorf("placement.rack: %w", err)
	}
	host, err := toProximity(b.Host)
	if err != nil {
		return weft.PlacementRule{}, fmt.Errorf("placement.host: %w", err)
	}
	return weft.PlacementRule{AZ: az, Rack: rack, Host: host}, nil
}

// GroupScheduleRequest builds the multi-replica scheduler input
// for a plan. `projectUUID` and `hypervisor` come from the
// deploy site (project resolution + driver dispatch). Other
// per-replica constraints (architecture, network types, …) are
// left empty — the placement-rule path is independent of
// per-host capability matching today ; richer constraints can
// drop in here as plan schemas grow.
func (p *Plan) GroupScheduleRequest(projectUUID, hypervisor string) (weft.GroupScheduleRequest, error) {
	rule, err := p.Placement.PlacementRule()
	if err != nil {
		return weft.GroupScheduleRequest{}, err
	}
	return weft.GroupScheduleRequest{
		ScheduleRequest: weft.ScheduleRequest{
			ProjectUUID: projectUUID,
			VMName:      "infra-" + p.Service,
			Hypervisor:  hypervisor,
		},
		Replicas:  p.ReplicaCount(),
		Placement: rule,
	}, nil
}

// toProximity maps an HCL-friendly string to the scheduler enum.
// Empty / unknown are surfaced as errors here — the loader's
// PlacementBlk.Validate() catches typos earlier, but we keep the
// check as belt-and-braces.
func toProximity(s string) (weft.Proximity, error) {
	switch s {
	case "":
		return weft.ProximityAny, nil
	case "same":
		return weft.ProximitySame, nil
	case "different":
		return weft.ProximityDifferent, nil
	}
	return "", fmt.Errorf("unknown proximity %q (want \"\" | \"same\" | \"different\")", s)
}
