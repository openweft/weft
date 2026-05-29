package weft

// hosts.go owns the cluster's Host inventory: one entry per
// vzd-agent that has registered with the control plane. The
// registry is **global** (not per-project): hosts are
// infrastructure shared across every tenant.
//
// Why this exists today, before any actual multi-host code:
// every other registry that's being designed (VM inventory,
// scheduler, port allocation) must already know how to refer to
// a host. Defining the data shape first means later commits can
// add `host_uuid` foreign keys without churn.
//
// Schema:
//
//   host "abc-…" {
//     hostname        = "compute-01"
//     az              = "us-east-1a"
//     endpoint        = "compute-01.internal:8443"
//     hypervisor      = "apple-vz"
//     architecture    = "arm64"
//     network_types   = ["nat", "bridged", "mesh"]
//     volume_backends = ["file"]
//     labels = {
//       gpu = "none"
//       ssd = "true"
//     }
//     state         = "active"        # active | draining | down
//     last_seen_at  = "..."           # heartbeat timestamp
//     created_at    = "..."
//   }
//
// Phase E of [[etcd-control-plane]]: stored alongside the other
// registries. Once vzd-agents come online, each agent calls
// RegisterHost on startup and SetHostHeartbeat on a timer; the
// control plane drifts a host to "down" when LastSeenAt ages
// past a configurable TTL.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// HostState enumerates the lifecycle stages an operator + the
// scheduler care about.
type HostState string

const (
	// HostStateActive — accepts new VM placements.
	HostStateActive HostState = "active"
	// HostStateDraining — no new placements; existing VMs keep
	// running. Set by `vzc host drain` for maintenance.
	HostStateDraining HostState = "draining"
	// HostStateDown — heartbeat aged past TTL OR explicit
	// `vzc host stop`. Scheduler treats as unavailable; VMs
	// scheduled here are candidates for failover.
	HostStateDown HostState = "down"
)

// Host is one compute node in the cluster.
//
// Placement hierarchy (per [[vzd-placement-rules]]):
//
//   AZ (availability zone / datacenter)
//     └── Rack (top-of-rack switch / power domain)
//           └── Host (this entry, one hypervisor instance)
//
// Anti-affinity at the Rack level protects against single-rack
// failure modes (ToR switch, PDU) that don't take out the whole
// AZ. Optional — single-rack dev clusters leave it empty.
type Host struct {
	UUID           string            `json:"uuid"`
	Hostname       string            `json:"hostname"`
	AZ             string            `json:"az"`
	Rack           string            `json:"rack,omitempty"`
	Endpoint       string            `json:"endpoint"` // host:port of the agent's gRPC listener
	Hypervisor     string            `json:"hypervisor"`
	Architecture   string            `json:"architecture"`
	NetworkTypes   []string          `json:"network_types"`
	VolumeBackends []string          `json:"volume_backends"`
	Labels         map[string]string `json:"labels,omitempty"`
	State          HostState         `json:"state"`
	LastSeenAt     time.Time         `json:"last_seen_at"`
	CreatedAt      time.Time         `json:"created_at"`
}

// hostsDoc is the top-level HCL schema.
type hostsDoc struct {
	Hosts []hostBlock `hcl:"host,block"`
}

type hostBlock struct {
	UUID           string            `hcl:",label"`
	Hostname       string            `hcl:"hostname"`
	AZ             string            `hcl:"az,optional"`
	Rack           string            `hcl:"rack,optional"`
	Endpoint       string            `hcl:"endpoint,optional"`
	Hypervisor     string            `hcl:"hypervisor,optional"`
	Architecture   string            `hcl:"architecture,optional"`
	NetworkTypes   []string          `hcl:"network_types,optional"`
	VolumeBackends []string          `hcl:"volume_backends,optional"`
	Labels         map[string]string `hcl:"labels,optional"`
	State          string            `hcl:"state,optional"`
	LastSeenAt     string            `hcl:"last_seen_at,optional"`
	CreatedAt      string            `hcl:"created_at"`
}

// hostRegistry mirrors projectRegistry / userRegistry — global
// scope (no projectIdx).
type hostRegistry struct {
	mu       sync.Mutex
	storage  Storage
	byUUID   map[string]Host
	nameIdx  map[string]string // hostname → UUID (hostnames must be unique cluster-wide)
}

func loadHostRegistry(ctx context.Context, storage Storage) (*hostRegistry, error) {
	reg := &hostRegistry{
		storage: storage,
		byUUID:  make(map[string]Host),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load host registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc hostsDoc
	if err := hclsimple.Decode("host-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse host registry: %w", err)
	}
	for _, b := range doc.Hosts {
		created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		lastSeen, _ := time.Parse(time.RFC3339Nano, b.LastSeenAt)
		state := HostState(b.State)
		if state == "" {
			state = HostStateActive
		}
		var labels map[string]string
		if len(b.Labels) > 0 {
			labels = make(map[string]string, len(b.Labels))
			for k, v := range b.Labels {
				labels[k] = v
			}
		}
		h := Host{
			UUID:           b.UUID,
			Hostname:       b.Hostname,
			AZ:             b.AZ,
			Rack:           b.Rack,
			Endpoint:       b.Endpoint,
			Hypervisor:     b.Hypervisor,
			Architecture:   b.Architecture,
			NetworkTypes:   append([]string(nil), b.NetworkTypes...),
			VolumeBackends: append([]string(nil), b.VolumeBackends...),
			Labels:         labels,
			State:          state,
			LastSeenAt:     lastSeen,
			CreatedAt:      created,
		}
		reg.byUUID[h.UUID] = h
		if h.Hostname != "" {
			reg.nameIdx[h.Hostname] = h.UUID
		}
	}
	return reg, nil
}

// saveLocked writes the registry. Caller holds mu.
func (r *hostRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# vzd host registry — cluster-wide compute node inventory.\n" +
				"# Each vzd-agent registers itself here on startup. UUID is\n" +
				"# immutable; hostname unique cluster-wide; state managed by\n" +
				"# the control plane (heartbeat → active/down).\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		h := r.byUUID[u]
		block := body.AppendNewBlock("host", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("hostname", cty.StringVal(h.Hostname))
		if h.AZ != "" {
			bb.SetAttributeValue("az", cty.StringVal(h.AZ))
		}
		if h.Rack != "" {
			bb.SetAttributeValue("rack", cty.StringVal(h.Rack))
		}
		if h.Endpoint != "" {
			bb.SetAttributeValue("endpoint", cty.StringVal(h.Endpoint))
		}
		if h.Hypervisor != "" {
			bb.SetAttributeValue("hypervisor", cty.StringVal(h.Hypervisor))
		}
		if h.Architecture != "" {
			bb.SetAttributeValue("architecture", cty.StringVal(h.Architecture))
		}
		if len(h.NetworkTypes) > 0 {
			vals := make([]cty.Value, len(h.NetworkTypes))
			for i, s := range h.NetworkTypes {
				vals[i] = cty.StringVal(s)
			}
			bb.SetAttributeValue("network_types", cty.ListVal(vals))
		}
		if len(h.VolumeBackends) > 0 {
			vals := make([]cty.Value, len(h.VolumeBackends))
			for i, s := range h.VolumeBackends {
				vals[i] = cty.StringVal(s)
			}
			bb.SetAttributeValue("volume_backends", cty.ListVal(vals))
		}
		if len(h.Labels) > 0 {
			ctyMap := make(map[string]cty.Value, len(h.Labels))
			for k, v := range h.Labels {
				ctyMap[k] = cty.StringVal(v)
			}
			bb.SetAttributeValue("labels", cty.MapVal(ctyMap))
		}
		if h.State != "" {
			bb.SetAttributeValue("state", cty.StringVal(string(h.State)))
		}
		if !h.LastSeenAt.IsZero() {
			bb.SetAttributeValue("last_seen_at", cty.StringVal(h.LastSeenAt.Format(time.RFC3339Nano)))
		}
		bb.SetAttributeValue("created_at", cty.StringVal(h.CreatedAt.Format(time.RFC3339Nano)))
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

func validateHostState(s HostState) error {
	switch s {
	case "", HostStateActive, HostStateDraining, HostStateDown:
		return nil
	default:
		return fmt.Errorf("unknown host state %q (want active, draining, or down)", s)
	}
}

// lookupByUUID returns (Host, true) when registered.
func (r *hostRegistry) lookupByUUID(uuid string) (Host, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.byUUID[uuid]
	return h, ok
}

// lookupByHostname returns (Host, true) by hostname. Hostnames
// are unique cluster-wide.
func (r *hostRegistry) lookupByHostname(hostname string) (Host, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[hostname]
	if !ok {
		return Host{}, false
	}
	h, ok := r.byUUID[uuid]
	return h, ok
}

// list returns every host, sorted by (AZ, Hostname) — natural
// tabular order for `vzc host list`.
func (r *hostRegistry) list() []Host {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Host, 0, len(r.byUUID))
	for _, h := range r.byUUID {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AZ != out[j].AZ {
			return out[i].AZ < out[j].AZ
		}
		return out[i].Hostname < out[j].Hostname
	})
	return out
}

// listByAZ returns hosts in the given availability zone. Empty
// AZ matches hosts with no AZ set.
func (r *hostRegistry) listByAZ(az string) []Host {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Host
	for _, h := range r.byUUID {
		if h.AZ == az {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out
}

// RegisterHostSpec is what an agent (or admin CLI) provides on
// first registration. When UUID is empty the registry mints a
// fresh one (admin CLI case); when UUID is set the registry
// treats the call as "this is who I am, register me OR refresh
// my entry" — see register() for the idempotent semantics.
//
// Setting UUID is how vzd-agents persist their identity across
// restarts: the agent loads its UUID from a host-local file
// (typically <stateDir>/host-uuid) and passes it here.
type RegisterHostSpec struct {
	UUID           string // empty → generate; set → idempotent re-register
	Hostname       string
	AZ             string
	Rack           string // optional sub-AZ placement tag; see [[vzd-placement-rules]]
	Endpoint       string
	Hypervisor     string
	Architecture   string
	NetworkTypes   []string
	VolumeBackends []string
	Labels         map[string]string
}

// register adds a new host or, when spec.UUID matches an
// existing entry, refreshes its mutable metadata (idempotent
// re-registration).
//
// Behaviour:
//
//   * spec.UUID == "" : mint a fresh UUID; reject if the
//     hostname is already taken by a different host.
//   * spec.UUID set, no existing host with that UUID : create
//     with the provided UUID; same hostname-uniqueness check.
//   * spec.UUID set, host already exists : refresh AZ, Endpoint,
//     Hypervisor, Architecture, NetworkTypes, VolumeBackends,
//     Labels; bump LastSeenAt; revive State if Down (operator
//     Draining is preserved). Hostname must match the existing
//     entry — changing it requires explicit Delete+register.
func (r *hostRegistry) register(spec RegisterHostSpec) (Host, error) {
	if spec.Hostname == "" {
		return Host{}, fmt.Errorf("empty hostname")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()

	// Idempotent re-register path: caller already knows its UUID.
	if spec.UUID != "" {
		if existing, ok := r.byUUID[spec.UUID]; ok {
			if existing.Hostname != spec.Hostname {
				return Host{}, fmt.Errorf("host %q hostname mismatch: registered as %q, re-register attempted as %q", spec.UUID, existing.Hostname, spec.Hostname)
			}
			existing.AZ = spec.AZ
			existing.Rack = spec.Rack
			existing.Endpoint = spec.Endpoint
			existing.Hypervisor = spec.Hypervisor
			existing.Architecture = spec.Architecture
			existing.NetworkTypes = append([]string(nil), spec.NetworkTypes...)
			existing.VolumeBackends = append([]string(nil), spec.VolumeBackends...)
			if len(spec.Labels) > 0 {
				existing.Labels = make(map[string]string, len(spec.Labels))
				for k, v := range spec.Labels {
					existing.Labels[k] = v
				}
			} else {
				existing.Labels = nil
			}
			existing.LastSeenAt = now
			if existing.State == HostStateDown {
				existing.State = HostStateActive
			}
			r.byUUID[spec.UUID] = existing
			if err := r.saveLocked(); err != nil {
				return Host{}, err
			}
			return existing, nil
		}
	}

	// Fresh registration. Reject hostname collisions.
	if _, taken := r.nameIdx[spec.Hostname]; taken {
		return Host{}, fmt.Errorf("hostname %q already registered", spec.Hostname)
	}
	uuid := spec.UUID
	if uuid == "" {
		uuid = newUUID()
	}
	var labels map[string]string
	if len(spec.Labels) > 0 {
		labels = make(map[string]string, len(spec.Labels))
		for k, v := range spec.Labels {
			labels[k] = v
		}
	}
	h := Host{
		UUID:           uuid,
		Hostname:       spec.Hostname,
		AZ:             spec.AZ,
		Rack:           spec.Rack,
		Endpoint:       spec.Endpoint,
		Hypervisor:     spec.Hypervisor,
		Architecture:   spec.Architecture,
		NetworkTypes:   append([]string(nil), spec.NetworkTypes...),
		VolumeBackends: append([]string(nil), spec.VolumeBackends...),
		Labels:         labels,
		State:          HostStateActive,
		LastSeenAt:     now,
		CreatedAt:      now,
	}
	r.byUUID[h.UUID] = h
	r.nameIdx[h.Hostname] = h.UUID
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, h.UUID)
		delete(r.nameIdx, h.Hostname)
		return Host{}, err
	}
	return h, nil
}

// heartbeat updates LastSeenAt and (when applicable) flips State
// back from Down to Active. Operator-set Draining is preserved
// — only Down is treated as "automatic, reversed by heartbeat".
func (r *hostRegistry) heartbeat(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("host %q not found", uuid)
	}
	h.LastSeenAt = time.Now().UTC()
	if h.State == HostStateDown {
		h.State = HostStateActive
	}
	r.byUUID[uuid] = h
	return r.saveLocked()
}

// setState transitions the host between active / draining / down.
// Used by the operator CLI (drain for maintenance) and by the
// control-plane TTL sweeper (mark down when heartbeat stales).
func (r *hostRegistry) setState(uuid string, state HostState) error {
	if err := validateHostState(state); err != nil {
		return err
	}
	if state == "" {
		return fmt.Errorf("empty state")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("host %q not found", uuid)
	}
	if h.State == state {
		return nil
	}
	h.State = state
	r.byUUID[uuid] = h
	return r.saveLocked()
}

// setLabels replaces the labels map atomically.
func (r *hostRegistry) setLabels(uuid string, labels map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("host %q not found", uuid)
	}
	if len(labels) > 0 {
		copy := make(map[string]string, len(labels))
		for k, v := range labels {
			copy[k] = v
		}
		h.Labels = copy
	} else {
		h.Labels = nil
	}
	r.byUUID[uuid] = h
	return r.saveLocked()
}

// delete drops a host. Refuses when the host is Active — operator
// must drain first, then delete. (Future commit: also refuse when
// any VM still has host_uuid == this host.)
func (r *hostRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("host %q not found", uuid)
	}
	if h.State == HostStateActive {
		return fmt.Errorf("host %q is active — drain before delete", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, h.Hostname)
	return r.saveLocked()
}
