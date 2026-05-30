// down.go is the inverse of `weft up`: it computes the convergence plan that
// tears a cluster down — stop replicas, stop the agents, wipe the overlay
// mesh — and (optionally) purges per-host state. The shape mirrors up so the
// same Plan/Apply infrastructure executes both directions; the new
// ActionKinds plug into renderAction in ssh.go.
package cluster

import (
	"github.com/openweft/weft/infra"
)

// Down-side ActionKinds. Kept on the same Plan struct so RenderSSH/Apply
// don't fork.
const (
	// StopReplica: delete a placed replica's microVM on its host. Inverse
	// of PlaceReplica — renders as `weft microvm rm <name>`.
	StopReplica ActionKind = "stop-replica"
	// StopAgent: terminate the weft agent on a host. Inverse of the
	// `nohup weft agent &` launch emitted by EnsureHost.
	StopAgent ActionKind = "stop-agent"
	// TeardownMesh: wipe WireGuard state on a host. Inverse of MeshSync.
	TeardownMesh ActionKind = "teardown-mesh"
	// Purge: remove all per-host weft state (host UUID, etcd data,
	// caches). Emitted only when DownOptions.Purge is set; without it
	// `weft up` can resume cleanly with the same host identity.
	Purge ActionKind = "purge"
)

// DownOptions tunes BuildDownPlan. Purge appends Purge actions per host that
// drop ~/.weft and /var/lib/weft after the mesh is torn down.
type DownOptions struct {
	Purge bool
}

// BuildDownPlan emits the convergence plan that reverses `weft up`:
//
//  1. StopReplica per placed replica (reverse topo order — dependents die
//     before what they depend on, so etcd outlives dex/nats until the end).
//  2. StopAgent per host.
//  3. TeardownMesh per host.
//  4. Purge per host when opts.Purge is set.
//
// infraOrder must be in dependency order (infra.TopologicalSort), same input
// as Build — we iterate it backwards. Re-running BuildDownPlan after a
// partial `weft down` is harmless: every action's rendered command is
// idempotent (rm is best-effort, pkill is `|| true`, wg-quick is `|| true`).
func BuildDownPlan(c *Cluster, infraOrder []*infra.Plan, opts DownOptions) (*Plan, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	hostCount := len(c.Hosts)
	topology := "single"
	if c.IsCluster() {
		topology = "cluster"
	}
	p := &Plan{Cluster: c.Name, Topology: topology}

	// 1. Stop replicas in reverse dependency order so dependents come down
	//    first and quorum services (etcd) die last. Same host placement
	//    formula as Build: replica r → c.Hosts[(r-1)%hostCount].
	for i := len(infraOrder) - 1; i >= 0; i-- {
		plan := infraOrder[i]
		eff := effectiveReplicas(plan, hostCount)
		for r := 1; r <= eff; r++ {
			h := c.Hosts[(r-1)%hostCount]
			p.Actions = append(p.Actions, Action{
				Kind:    StopReplica,
				Service: plan.Service,
				Replica: r,
				Host:    h.ID,
				DC:      h.DC,
				// Carry the VM name so the renderer doesn't have to reach
				// back into the infra.Plan to derive it.
				Image: plan.VMNameFor(r),
			})
		}
	}

	// 2. Stop the agent on every host (seed last so cross-host SSH from
	//    the seed isn't needed — each StopAgent runs on its own host).
	for _, h := range c.Hosts {
		p.Actions = append(p.Actions, Action{Kind: StopAgent, Host: h.ID, DC: h.DC})
	}

	// 3. Tear down the WireGuard overlay on every host.
	for _, h := range c.Hosts {
		p.Actions = append(p.Actions, Action{Kind: TeardownMesh, Host: h.ID, DC: h.DC})
	}

	// 4. Optionally purge per-host state (UUID, etcd data, caches).
	if opts.Purge {
		for _, h := range c.Hosts {
			p.Actions = append(p.Actions, Action{Kind: Purge, Host: h.ID, DC: h.DC})
		}
	}

	return p, nil
}
