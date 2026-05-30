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
	// EnsureHost: bring a hypervisor host into the cluster (start/verify its
	// weft agent, join it to the overlay).
	EnsureHost ActionKind = "ensure-host"
	// MeshSync: (re)publish the full WireGuard peer set after membership
	// changed — reuses wgcoord.MeshPeers + mesh.PublishAll.
	MeshSync ActionKind = "mesh-sync"
	// EnsureKernel: stage the shared microVM kernel binary on a host by
	// pulling its OCI artifact (cluster.Microvm.KernelRef). Emitted once
	// per host that runs any microVM-backed service, before EnsureImage.
	EnsureKernel ActionKind = "ensure-kernel"
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
	Host     string // EnsureHost, PlaceReplica, EnsureImage
	DC       string
	Service  string // PlaceReplica, GrowQuorum
	Replica  int    // PlaceReplica (1-indexed)
	From, To int    // GrowQuorum
	Hosts    []string // MeshSync (the full desired member set)
	Image    string // EnsureImage (OCI ref, e.g. quay.io/coreos/etcd:v3.6.0)
}

func (a Action) String() string {
	switch a.Kind {
	case EnsureHost:
		return fmt.Sprintf("ensure-host   %s (dc=%s)", a.Host, a.DC)
	case MeshSync:
		return fmt.Sprintf("mesh-sync     members=[%s]", strings.Join(a.Hosts, ","))
	case EnsureKernel:
		return fmt.Sprintf("ensure-kernel %s on %s", a.Image, a.Host)
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

	// 1. Bring up any host not yet in the cluster.
	var newHosts bool
	for _, h := range c.Hosts {
		if !cur.hostUp(h.ID) {
			p.Actions = append(p.Actions, Action{Kind: EnsureHost, Host: h.ID, DC: h.DC})
			newHosts = true
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
	kernelRef := ""
	if c.Microvm != nil {
		kernelRef = c.Microvm.KernelRef
	}
	for _, plan := range infraOrder {
		eff := effectiveReplicas(plan, hostCount)
		for r := 1; r <= eff; r++ {
			h := c.Hosts[(r-1)%hostCount]
			if cur, ok := cur.replicaHost(plan.Service, r); ok && cur == h.ID {
				continue // already placed correctly
			}
			if kernelRef != "" && !kernelOnHost[h.ID] {
				p.Actions = append(p.Actions, Action{
					Kind: EnsureKernel, Host: h.ID, Image: kernelRef,
				})
				kernelOnHost[h.ID] = true
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
