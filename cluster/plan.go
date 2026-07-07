package cluster

import (
	"fmt"
	"sort"
	"strings"

	"github.com/openweft/weft/infra"
)

// State is the observed cluster state the planner reconciles against. The
// zero value (no hosts up, nothing placed) yields a full bootstrap; a
// partial state yields the delta — which is how "extend a single host to a
// 3-DC cluster" falls out of the same planner.
type State struct {
	Hosts  map[string]bool           // host IDs currently up (agent reachable + meshed)
	Placed map[string]map[int]string // service → replica(1-indexed) → host ID currently placed
	// AZs : codes already present in the inventory (CreateAZ has
	// been called). Populated by Apply() from the seed agent before
	// Build runs ; the planner uses it to skip emitting EnsureAZ
	// for already-present codes.
	AZs map[string]bool
	// Racks : "<DC>/<rack>" tuples already present. Same shape +
	// semantics as AZs.
	Racks map[string]bool
}

func (s State) hostUp(id string) bool { return s.Hosts != nil && s.Hosts[id] }

func (s State) replicaHost(service string, replica int) (string, bool) {
	if s.Placed == nil {
		return "", false
	}
	h, ok := s.Placed[service][replica]
	return h, ok
}

func (s State) replicaCount(service string) int {
	if s.Placed == nil {
		return 0
	}
	return len(s.Placed[service])
}

// ActionKind enumerates the convergence steps.
type ActionKind string

const (
	// PushAgentConfig: write /etc/weft/weft.hcl on a host with the rendered
	// agent_config block from cluster.hcl (merged with any per-host
	// override). Emitted BEFORE EnsureHost so the file is in place when
	// weft-agent starts.
	PushAgentConfig ActionKind = "push-agent-config"
	// EnsureHost: bring a hypervisor host into the cluster (start/verify its
	// weft agent, join it to the overlay).
	EnsureHost ActionKind = "ensure-host"
	// EnsureAZ : create the AZ inventory record from cluster.hcl's
	// distinct host.DC values. Runs against the seed agent so the
	// records exist in etcd before hosts self-register and reference
	// them by code. Idempotent — re-applying skips an existing AZ.
	// Without this step the host registry is populated but the AZ +
	// Rack list-panels stay empty (operator-reported bug 2026-06-23
	// "dans la TUI il manque toujours les AZ et les racks").
	EnsureAZ ActionKind = "ensure-az"
	// EnsureRack : same shape as EnsureAZ but for the (DC, rack)
	// tuples in cluster.hcl. Emitted AFTER its parent EnsureAZ so
	// the --az lookup resolves.
	EnsureRack ActionKind = "ensure-rack"
	// MeshSync: (re)publish the full WireGuard peer set after membership
	// changed — reuses wgcoord.MeshPeers + mesh.PublishAll.
	MeshSync ActionKind = "mesh-sync"
	// EnsureKernel: stage the shared microVM kernel binary on a host by
	// pulling its OCI artifact (cluster.Microvm.KernelRef). Emitted once
	// per host that runs any microVM-backed service, before EnsureImage.
	EnsureKernel ActionKind = "ensure-kernel"
	// EnsureInitrd: stage the shared pod-mode initramfs (weft-init + crun)
	// on a host by pulling its OCI artifact (cluster.Microvm.PodInitrdRef).
	// Emitted once per host that runs any microVM-backed service, alongside
	// EnsureKernel, before EnsureImage. Without it, pod-mode microVMs boot
	// to a weft-init that can't exec crun and call poweroff inside a second
	// (see project_weft_3dc_live_cluster bug 6).
	EnsureInitrd ActionKind = "ensure-initrd"
	// EnsureImage: pre-pull a service's OCI rootfs onto a host so the
	// subsequent PlaceReplica `weft infra deploy` finds it in the local
	// weft-microvm image cache. One action per distinct (host, image) pair,
	// k0sctl-style "Prepare" phase.
	EnsureImage ActionKind = "ensure-image"
	// PlaceReplica: deploy replica N of a service on a host (via the per-host
	// `weft infra` deploy of that service's plan).
	PlaceReplica ActionKind = "place-replica"
	// GrowQuorum: extend an already-running quorum service (etcd/nats) from
	// From to To members — etcd member-add + initial-cluster-state=existing.
	GrowQuorum ActionKind = "grow-quorum"
)

// Action is one ordered convergence step.
type Action struct {
	Kind     ActionKind
	Host     string // EnsureHost, PlaceReplica, EnsureImage, PushAgentConfig
	DC       string
	Service  string // PlaceReplica, GrowQuorum
	Replica  int    // PlaceReplica (1-indexed)
	From, To int    // GrowQuorum
	Hosts    []string // MeshSync (the full desired member set)
	Image    string // EnsureImage (OCI ref, e.g. quay.io/coreos/etcd:v3.6.0)
	// Config is the rendered weft.hcl content for PushAgentConfig — carried
	// here so renderAction is pure (no second pass over Cluster needed).
	Config string
	// Properties is the desired Host.Properties map carried on an
	// EnsureHost action so the executor can mirror cluster.hcl's
	// declared properties into the runtime host registry (e.g. via
	// SetHostLabels — properties get persisted in the same map the
	// runtime calls "labels" because there's only one map at the
	// registry level). Nil = no properties declared in cluster.hcl.
	Properties map[string]string
}

func (a Action) String() string {
	switch a.Kind {
	case PushAgentConfig:
		return fmt.Sprintf("push-cfg      %s (/etc/weft/weft.hcl, %d bytes)", a.Host, len(a.Config))
	case EnsureHost:
		return fmt.Sprintf("ensure-host   %s (dc=%s)", a.Host, a.DC)
	case EnsureAZ:
		return fmt.Sprintf("ensure-az     %s (region=%s)", a.DC, a.Service)
	case EnsureRack:
		return fmt.Sprintf("ensure-rack   %s/%s", a.DC, a.Service)
	case MeshSync:
		return fmt.Sprintf("mesh-sync     members=[%s]", strings.Join(a.Hosts, ","))
	case EnsureKernel:
		return fmt.Sprintf("ensure-kernel %s on %s", a.Image, a.Host)
	case EnsureInitrd:
		return fmt.Sprintf("ensure-initrd %s on %s", a.Image, a.Host)
	case EnsureImage:
		return fmt.Sprintf("ensure-image  %s on %s", a.Image, a.Host)
	case PlaceReplica:
		return fmt.Sprintf("place-replica %s/%d → %s (dc=%s)", a.Service, a.Replica, a.Host, a.DC)
	case GrowQuorum:
		return fmt.Sprintf("grow-quorum   %s %d→%d", a.Service, a.From, a.To)
	case StopReplica:
		return fmt.Sprintf("stop-replica  %s/%d on %s (dc=%s)", a.Service, a.Replica, a.Host, a.DC)
	case StopAgent:
		return fmt.Sprintf("stop-agent    %s", a.Host)
	case TeardownMesh:
		return fmt.Sprintf("teardown-mesh %s", a.Host)
	case Purge:
		return fmt.Sprintf("purge         %s (~/.weft, /var/lib/weft)", a.Host)
	default:
		return string(a.Kind)
	}
}

// Plan is the ordered, idempotent set of actions to converge the cluster to
// the desired state. An empty Actions slice means already-converged.
type Plan struct {
	Cluster  string
	Topology string // "single" | "cluster"
	Actions  []Action
}

func (p *Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cluster %q (%s) — %d action(s)\n", p.Cluster, p.Topology, len(p.Actions))
	if len(p.Actions) == 0 {
		b.WriteString("  (already converged)\n")
	}
	for i, a := range p.Actions {
		fmt.Fprintf(&b, "  %2d. %s\n", i+1, a.String())
	}
	return b.String()
}

// declaredReplicas is the operator-declared replica count of an infra plan:
// the max of an explicit placement.count, the number of pre-assigned static
// IPs (a 3-IP network block means a 3-replica service), and 1.
func declaredReplicas(p *infra.Plan) int {
	n := p.ReplicaCount()
	if p.Network != nil && len(p.Network.StaticIP) > n {
		n = len(p.Network.StaticIP)
	}
	if n < 1 {
		n = 1
	}
	return n
}

// effectiveReplicas is how many replicas actually run given the host count:
// a single-node deploy collapses every service to one replica (no quorum);
// a multi-host deploy runs up to one replica per host.
func effectiveReplicas(p *infra.Plan, hostCount int) int {
	if hostCount <= 1 {
		return 1
	}
	if d := declaredReplicas(p); d < hostCount {
		return d
	}
	return hostCount
}

// Build computes the convergence plan: desired (from the cluster topology +
// the topo-sorted infra DAG) minus the observed State. infraOrder must be in
// dependency order (infra.TopologicalSort). cur may be the zero State for a
// fresh bootstrap.
//
// Infra-service placement honors cluster.ControlPlane: when require_properties is
// set, replicas are round-robined across the SUBSET of c.Hosts whose Labels
// satisfy every entry. If the eligible pool is empty Build returns an error
// — silently spilling onto a non-control-plane host would defeat the point
// of declaring the constraint.
//
// Wrap-around — when there are MORE replicas than eligible hosts (e.g. a
// 3-replica etcd against 2 control-plane hosts) the planner round-robins :
// replicas 1,2,3 → eligible[0], eligible[1], eligible[0]. Two replicas of
// the same service end up on the same host, which is degenerate for quorum
// services (etcd / nats) but is the right behavior for a 3-host cluster
// where the operator has explicitly chosen to label only 2 ; growing the
// label set or the host count is the operator's next step.
func Build(c *Cluster, infraOrder []*infra.Plan, cur State) (*Plan, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	hostCount := len(c.Hosts)
	topology := "single"
	if c.IsCluster() {
		topology = "cluster"
	}
	p := &Plan{Cluster: c.Name, Topology: topology}

	// Resolve the eligible host pool for infra placement. Empty pool with a
	// non-empty constraint is a hard error — fail loud, see godoc above.
	placementHosts, err := c.EligibleControlPlaneHosts()
	if err != nil {
		return nil, err
	}
	if c.ControlPlane != nil && len(c.ControlPlane.RequireProperties) > 0 && len(placementHosts) == 0 {
		return nil, fmt.Errorf(
			"cluster %q: control_plane.require_properties = %v but no host satisfies them ; add `labels = { %s }` to at least one host block",
			c.Name, c.ControlPlane.RequireProperties, c.ControlPlane.RequireProperties[0],
		)
	}

	// 1. Bring up any host not yet in the cluster. For each, push the
	//    rendered weft.hcl FIRST (so it's on disk when the agent reads
	//    its config on startup), then EnsureHost. Hosts with no
	//    agent_config at any level skip the push and keep whatever
	//    skeleton cloud-init wrote. EnsureHost also carries the host's
	//    declared labels so the executor pushes them to the runtime host
	//    registry via SetHostLabels.
	var newHosts bool
	for i := range c.Hosts {
		h := &c.Hosts[i]
		if cur.hostUp(h.ID) {
			continue
		}
		cfg := c.AgentConfigFor(h)
		if !cfg.IsEmpty() {
			p.Actions = append(p.Actions, Action{
				Kind:   PushAgentConfig,
				Host:   h.ID,
				DC:     h.DC,
				Config: cfg.RenderHCL(),
			})
		}
		p.Actions = append(p.Actions, Action{
			Kind:   EnsureHost,
			Host:   h.ID,
			DC:     h.DC,
			Properties: h.Properties,
		})
		newHosts = true
	}

	// 1bis. Ensure the inventory records (AZ + Rack) referenced by
	// host.DC + host.Rack exist. The Host registry takes the codes
	// as free-text labels regardless ; without these records, the
	// TUI/webui's AZ + Rack list-panels stay empty even though the
	// hosts show their AZ/Rack columns populated. Idempotent —
	// `weft az create` / `weft rack create` skip when the code
	// already exists. Runs on the seed (the only place that has
	// the agent socket guaranteed up by this point). One action per
	// distinct DC + per distinct (DC, rack) tuple.
	azSeen := map[string]bool{}
	rackSeen := map[string]bool{}
	for _, h := range c.Hosts {
		if h.DC != "" && !azSeen[h.DC] && !cur.AZs[h.DC] {
			azSeen[h.DC] = true
			p.Actions = append(p.Actions, Action{Kind: EnsureAZ, DC: h.DC, Host: c.Seed().ID})
		}
		if h.DC != "" && h.Rack != "" {
			key := h.DC + "/" + h.Rack
			if !rackSeen[key] && !cur.Racks[key] {
				rackSeen[key] = true
				p.Actions = append(p.Actions, Action{Kind: EnsureRack, DC: h.DC, Service: h.Rack, Host: c.Seed().ID})
			}
		}
	}

	// 2. Re-sync the overlay whenever membership grew.
	if newHosts {
		ids := make([]string, len(c.Hosts))
		for i, h := range c.Hosts {
			ids[i] = h.ID
		}
		p.Actions = append(p.Actions, Action{Kind: MeshSync, Hosts: ids})
	}

	// 3. Place each service's replicas in dependency order; skip any already
	//    on the right host (idempotent). For each host that runs at least one
	//    service we also emit:
	//      EnsureKernel (once, if microvm.kernel_ref is configured)
	//      EnsureImage  (once per distinct OCI rootfs)
	//    before the first PlaceReplica that targets it. EnsureKernel comes
	//    first so the kernel is on disk when RegisterMicroVM copies it into
	//    the per-VM directory.
	pulled := map[string]bool{}        // "<host>|<image>" → EnsureImage already emitted
	kernelOnHost := map[string]bool{}  // "<host>" → EnsureKernel already emitted
	initrdOnHost := map[string]bool{}  // "<host>" → EnsureInitrd already emitted
	kernelRef, initrdRef := "", ""
	if c.Microvm != nil {
		kernelRef = c.Microvm.KernelRef
		initrdRef = c.Microvm.PodInitrdRef
	}
	for _, plan := range infraOrder {
		eff := effectiveReplicas(plan, hostCount)
		for r := 1; r <= eff; r++ {
			// Round-robin over the eligible pool (which is c.Hosts when
			// control_plane is absent — identical to the legacy behavior).
			h := placementHosts[(r-1)%len(placementHosts)]
			if cur, ok := cur.replicaHost(plan.Service, r); ok && cur == h.ID {
				continue // already placed correctly
			}
			if kernelRef != "" && !kernelOnHost[h.ID] {
				p.Actions = append(p.Actions, Action{
					Kind: EnsureKernel, Host: h.ID, Image: kernelRef,
				})
				kernelOnHost[h.ID] = true
			}
			if initrdRef != "" && !initrdOnHost[h.ID] {
				p.Actions = append(p.Actions, Action{
					Kind: EnsureInitrd, Host: h.ID, Image: initrdRef,
				})
				initrdOnHost[h.ID] = true
			}
			if plan.OCIImage != "" {
				key := h.ID + "|" + plan.OCIImage
				if !pulled[key] {
					p.Actions = append(p.Actions, Action{
						Kind: EnsureImage, Host: h.ID, Image: plan.OCIImage,
					})
					pulled[key] = true
				}
			}
			p.Actions = append(p.Actions, Action{
				Kind: PlaceReplica, Service: plan.Service, Replica: r, Host: h.ID, DC: h.DC,
			})
		}
	}

	// 4. Grow already-running quorum services that gained replicas (etcd/nats
	//    1→3). from==0 is a fresh bootstrap (initial-cluster-state=new), not a
	//    grow, so it's skipped here.
	for _, plan := range infraOrder {
		if declaredReplicas(plan) <= 1 {
			continue // singleton, never a quorum
		}
		eff := effectiveReplicas(plan, hostCount)
		from := cur.replicaCount(plan.Service)
		if from > 0 && eff > from {
			p.Actions = append(p.Actions, Action{Kind: GrowQuorum, Service: plan.Service, From: from, To: eff})
		}
	}

	return p, nil
}

// ResolveServices returns the infra service names to deploy: the cluster's
// explicit list, or all discovered under <moduleRoot>/infra when unset.
func (c *Cluster) ResolveServices(moduleRoot string) ([]string, error) {
	if c.Infra != nil && len(c.Infra.Services) > 0 {
		out := append([]string(nil), c.Infra.Services...)
		sort.Strings(out)
		return out, nil
	}
	return infra.ListServices(moduleRoot)
}
