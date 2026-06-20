package weft

// hosts.go owns the cluster's Host inventory: one entry per
// weft-agent that has registered with the control plane. The
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
//     properties = {
//       gpu = "none"
//       ssd = "true"
//     }
//     state         = "active"        # active | draining | down
//     last_seen_at  = "..."           # heartbeat timestamp
//     created_at    = "..."
//   }
//
// Phase E of [[etcd-control-plane]]: stored alongside the other
// registries. Once weft-agents come online, each agent calls
// RegisterHost on startup and SetHostHeartbeat on a timer; the
// control plane drifts a host to "down" when LastSeenAt ages
// past a configurable TTL.

import (
	"context"
	"fmt"
	"os"
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
	// running. Set by `weft host drain` for maintenance.
	HostStateDraining HostState = "draining"
	// HostStateDown — heartbeat aged past TTL OR explicit
	// `weft host stop`. Scheduler treats as unavailable; VMs
	// scheduled here are candidates for failover.
	HostStateDown HostState = "down"
)

// staleHostTakeoverAge bounds when a hostname collision counts as
// "the prior agent is dead, take over" vs "two live agents are
// fighting for the same hostname, refuse". The heartbeat timer is
// typically 30s, so 5 minutes is 10 missed heartbeats : clearly
// dead. Operators who want a more aggressive policy can shrink it
// via the WEFT_STALE_HOST_TAKEOVER env override.
//
// The takeover branch in hostRegistry.register also accepts
// State==Down as a positive signal (the dispatch-session sweeper
// already flipped the host out of Active), so this threshold only
// matters for hosts that died WITHOUT going through the sweeper —
// the common "operator re-imaged the box, state/ was wiped" case.
var staleHostTakeoverAge = func() time.Duration {
	if v := os.Getenv("WEFT_STALE_HOST_TAKEOVER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
}()

// Host is one compute node in the cluster.
//
// Placement hierarchy (per [[weft-placement-rules]]):
//
//	AZ (availability zone / datacenter)
//	  └── Rack (top-of-rack switch / power domain)
//	        └── Host (this entry, one hypervisor instance)
//
// Anti-affinity at the Rack level protects against single-rack
// failure modes (ToR switch, PDU) that don't take out the whole
// AZ. Optional — single-rack dev clusters leave it empty.
type Host struct {
	UUID     string `json:"uuid"`
	Hostname string `json:"hostname"`
	AZ       string `json:"az"`
	Rack     string `json:"rack,omitempty"`
	Endpoint string `json:"endpoint"` // host:port of the agent's gRPC listener
	// Hypervisor / Architecture are the legacy single-driver fields ;
	// today every registration writes them as a singleton "primary"
	// driver (the first entry of Drivers when Drivers is non-empty).
	// Kept on the wire for backward-compat with schedulers / clients
	// that don't know about Drivers yet.
	Hypervisor   string `json:"hypervisor"`
	Architecture string `json:"architecture"`
	// Drivers is the full capability list — one entry per
	// weft-driver-<kind> subprocess this host's weft-agent has
	// launched, with the set of guest archs it can run. The scheduler
	// matches ScheduleRequest.{Hypervisor,Architecture} against this
	// list ; single-driver hosts get a single entry mirroring the
	// legacy fields, multi-driver hosts (an Apple Silicon machine
	// running both VZ + QEMU for cross-arch builds) get one entry per
	// driver.
	Drivers        []HostDriver `json:"drivers,omitempty"`
	NetworkTypes   []string     `json:"network_types"`
	VolumeBackends []string     `json:"volume_backends"`
	// GPUs is the physical accelerator inventory the host carries.
	// Populated by the agent at registration time via detectGPUs()
	// (currently a static stub — see gpu.go). The scheduler reads
	// this when matching ScheduleRequest.GPU (single-axis) and
	// ScheduleRequest.RequestedGPUs (fine-grained per-VM). Empty
	// = host has no GPUs ; scheduler treats the host as
	// `gpu="none"` for SchedulingRule matching.
	GPUs []GPU `json:"gpus,omitempty"`
	// PCIDevices is the physical PCI inventory the host carries, beyond
	// the GPU axis (NICs, NVMe, sound cards, FPGAs, …). Populated by the
	// agent at registration time via detectPCI() — today a Linux-only
	// stub (see pci.go) ; operators can still seed the inventory via the
	// static RegisterHostSpec.PCIDevices path until detection lands.
	// The scheduler reads this when matching ScheduleRequest.RequestedPCI
	// (see pci.go's pciRequestSatisfied).
	PCIDevices []PCIDevice       `json:"pci_devices,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
	State      HostState         `json:"state"`
	// Cordoned, when true, takes this host out of the scheduler's
	// candidate set for **new** placements ; already-running VMs stay
	// put. Independent of State — a cordoned host stays Active +
	// reachable, it just refuses new work. Set / cleared with
	// `weft host cordon` / `weft host uncordon`. Implements the
	// upgrade-runbook primitive previously documented as proposed.
	Cordoned   bool      `json:"cordoned,omitempty"`
	LastSeenAt time.Time `json:"last_seen_at"`
	CreatedAt  time.Time `json:"created_at"`
	// AKName is the TPM Attestation Key Name the host's node proved at
	// admission time (hex of the attest AK Name), stamped here when the
	// attestation gate is enabled. Empty for hosts registered through the
	// legacy OIDC-only path (the default) — attestation is feature-flagged
	// OFF, so this is unset on every existing host. Informational on the
	// registry side ; the gate decision happens in the RegisterHost RPC.
	AKName string `json:"ak_name,omitempty"`
	// WGPublicKey is the host's WireGuard public key for the host-level
	// overlay mesh (cluster prereq #3). The host MINTS its keypair locally
	// (EnsureHostWGKey) and registers ONLY the public half — the private key
	// never leaves the node, so the registry (and any NATS distribution built
	// from it) carries no secret. Empty on every host registered before the
	// host-mesh feature landed and on hosts that didn't mint a WG identity ;
	// such hosts simply don't participate in the host mesh (additive,
	// legacy-safe). When the attestation gate is enabled only attested nodes
	// reach RegisterHost, so a non-empty WGPublicKey is implicitly
	// attestation-gated.
	WGPublicKey string `json:"wg_public_key,omitempty"`
	// WGOverlayIndex is the host's stable 1-based index in the overlay
	// subnet ; wgcoord.Allocator.Host(idx) maps it to the host's overlay IP
	// deterministically, so the IP itself isn't persisted. Zero means
	// "unassigned" (legacy / non-mesh host) — HostMeshMembers skips such
	// hosts. Assigned by selfRegisterHost from a host-local file so it's
	// stable across restarts.
	WGOverlayIndex int `json:"wg_overlay_index,omitempty"`
}

// HostDriver is one weft-driver-<kind> subprocess running on a host, with
// the set of guest architectures it can launch. Mirrors cluster.HostDriver
// (cluster.hcl side) on the registry side — separation matters because the
// registry is updated by the AGENT at runtime, not by the operator's HCL.
type HostDriver struct {
	Kind   string   `json:"kind"`             // "vz" | "qemu"
	Arches []string `json:"arches,omitempty"` // "arm64" | "amd64" | "riscv64" | "loongarch64"
}

// SupportsArch reports whether this host can launch a guest with the given
// arch under the given driver kind. Falls back to the legacy
// Hypervisor / Architecture singleton when Drivers is empty.
func (h Host) SupportsArch(driverKind, arch string) bool {
	if len(h.Drivers) > 0 {
		for _, d := range h.Drivers {
			if d.Kind != driverKind {
				continue
			}
			for _, a := range d.Arches {
				if a == arch {
					return true
				}
			}
		}
		return false
	}
	// Legacy single-driver registration.
	return driverKind == h.Hypervisor && arch == h.Architecture
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
	Drivers        []hostDriverBlock `hcl:"driver,block"`
	NetworkTypes   []string          `hcl:"network_types,optional"`
	VolumeBackends []string          `hcl:"volume_backends,optional"`
	GPUs           []gpuBlock        `hcl:"gpu,block"`
	PCIDevices     []pciBlock        `hcl:"pci,block"`
	Properties     map[string]string `hcl:"properties,optional"`
	State          string            `hcl:"state,optional"`
	Cordoned       bool              `hcl:"cordoned,optional"`
	LastSeenAt     string            `hcl:"last_seen_at,optional"`
	CreatedAt      string            `hcl:"created_at"`
	AKName         string            `hcl:"ak_name,optional"`
	WGPublicKey    string            `hcl:"wg_public_key,optional"`
	WGOverlayIndex int               `hcl:"wg_overlay_index,optional"`
}

type hostDriverBlock struct {
	Kind   string   `hcl:",label"`
	Arches []string `hcl:"arches,optional"`
}

// gpuBlock mirrors the GPU struct on the HCL side. The block label
// is the canonical SKU string ("H200" / "RTX-6000-Ada") — humans
// edit the file by SKU first. Vendor + per-card details follow
// inside the block.
type gpuBlock struct {
	Model        string `hcl:",label"`
	Vendor       string `hcl:"vendor"`
	MemoryGiB    int    `hcl:"memory_gib,optional"`
	MIGCapable   bool   `hcl:"mig_capable,optional"`
	NVLinkDomain string `hcl:"nvlink_domain,optional"`
}

// hostRegistry mirrors projectRegistry / userRegistry — global
// scope (no projectIdx).
type hostRegistry struct {
	mu      sync.Mutex
	storage Storage   // blob backend (legacy / file / mem)
	kv      KVStorage // per-record backend (V0.1.5 etcd KV) — nil if blob-mode
	byUUID  map[string]Host
	nameIdx map[string]string // hostname → UUID (hostnames must be unique cluster-wide)
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
		var properties map[string]string
		if len(b.Properties) > 0 {
			properties = make(map[string]string, len(b.Properties))
			for k, v := range b.Properties {
				properties[k] = v
			}
		}
		var drivers []HostDriver
		if len(b.Drivers) > 0 {
			drivers = make([]HostDriver, 0, len(b.Drivers))
			for _, d := range b.Drivers {
				drivers = append(drivers, HostDriver{
					Kind:   d.Kind,
					Arches: append([]string(nil), d.Arches...),
				})
			}
		}
		var gpus []GPU
		if len(b.GPUs) > 0 {
			gpus = make([]GPU, 0, len(b.GPUs))
			for _, g := range b.GPUs {
				gpus = append(gpus, GPU{
					Vendor:       g.Vendor,
					Model:        g.Model,
					MemoryGiB:    g.MemoryGiB,
					MIGCapable:   g.MIGCapable,
					NVLinkDomain: g.NVLinkDomain,
				})
			}
		}
		var pciDevs []PCIDevice
		if len(b.PCIDevices) > 0 {
			pciDevs = make([]PCIDevice, 0, len(b.PCIDevices))
			for _, p := range b.PCIDevices {
				pciDevs = append(pciDevs, PCIDevice{
					BDF:      p.BDF,
					VendorID: p.VendorID,
					DeviceID: p.DeviceID,
					Driver:   p.Driver,
				})
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
			Drivers:        drivers,
			NetworkTypes:   append([]string(nil), b.NetworkTypes...),
			VolumeBackends: append([]string(nil), b.VolumeBackends...),
			GPUs:           gpus,
			PCIDevices:     pciDevs,
			Properties:     properties,
			State:          state,
			Cordoned:       b.Cordoned,
			LastSeenAt:     lastSeen,
			CreatedAt:      created,
			AKName:         b.AKName,
			WGPublicKey:    b.WGPublicKey,
			WGOverlayIndex: b.WGOverlayIndex,
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
			"# weft host registry — cluster-wide compute node inventory.\n" +
				"# Each weft-agent registers itself here on startup. UUID is\n" +
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
		// Drivers : one nested `driver "kind" { arches = [...] }` block per
		// capability entry. Single-driver hosts (legacy registrations) skip
		// this — Hypervisor/Architecture above carry the same information.
		for _, d := range h.Drivers {
			db := bb.AppendNewBlock("driver", []string{d.Kind}).Body()
			if len(d.Arches) > 0 {
				vals := make([]cty.Value, len(d.Arches))
				for i, a := range d.Arches {
					vals[i] = cty.StringVal(a)
				}
				db.SetAttributeValue("arches", cty.ListVal(vals))
			}
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
		// GPUs : one nested `gpu "<model>" { vendor = ..., memory_gib = ..., mig_capable = ... }`
		// block per accelerator. Empty inventory writes nothing — the
		// host's GPU axis defaults to "none" for SchedulingRule matching.
		for _, g := range h.GPUs {
			gb := bb.AppendNewBlock("gpu", []string{g.Model}).Body()
			gb.SetAttributeValue("vendor", cty.StringVal(g.Vendor))
			if g.MemoryGiB > 0 {
				gb.SetAttributeValue("memory_gib", cty.NumberIntVal(int64(g.MemoryGiB)))
			}
			if g.MIGCapable {
				gb.SetAttributeValue("mig_capable", cty.BoolVal(true))
			}
		}
		// PCIDevices : one nested `pci "<bdf>" { vendor_id, device_id, driver }`
		// per device. The BDF (bus:device.function) is the block label
		// because BDFs are the only unique key inside one host. Vendor /
		// device IDs are the 4-hex-digit PCI Code IDs the operator wires
		// into SchedulingRule's RequestedPCI.
		for _, p := range h.PCIDevices {
			pb := bb.AppendNewBlock("pci", []string{p.BDF}).Body()
			if p.VendorID != "" {
				pb.SetAttributeValue("vendor_id", cty.StringVal(p.VendorID))
			}
			if p.DeviceID != "" {
				pb.SetAttributeValue("device_id", cty.StringVal(p.DeviceID))
			}
			if p.Driver != "" {
				pb.SetAttributeValue("driver", cty.StringVal(p.Driver))
			}
		}
		if len(h.Properties) > 0 {
			ctyMap := make(map[string]cty.Value, len(h.Properties))
			for k, v := range h.Properties {
				ctyMap[k] = cty.StringVal(v)
			}
			bb.SetAttributeValue("properties", cty.MapVal(ctyMap))
		}
		if h.State != "" {
			bb.SetAttributeValue("state", cty.StringVal(string(h.State)))
		}
		if h.Cordoned {
			bb.SetAttributeValue("cordoned", cty.BoolVal(true))
		}
		if !h.LastSeenAt.IsZero() {
			bb.SetAttributeValue("last_seen_at", cty.StringVal(h.LastSeenAt.Format(time.RFC3339Nano)))
		}
		bb.SetAttributeValue("created_at", cty.StringVal(h.CreatedAt.Format(time.RFC3339Nano)))
		if h.AKName != "" {
			bb.SetAttributeValue("ak_name", cty.StringVal(h.AKName))
		}
		// Host-mesh WG identity — persisted only when the host minted one,
		// keeping the legacy / non-mesh path byte-identical.
		if h.WGPublicKey != "" {
			bb.SetAttributeValue("wg_public_key", cty.StringVal(h.WGPublicKey))
		}
		if h.WGOverlayIndex != 0 {
			bb.SetAttributeValue("wg_overlay_index", cty.NumberIntVal(int64(h.WGOverlayIndex)))
		}
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
// tabular order for `weft host list`.
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
// Setting UUID is how weft-agents persist their identity across
// restarts: the agent loads its UUID from a host-local file
// (typically <stateDir>/host-uuid) and passes it here.
type RegisterHostSpec struct {
	UUID         string // empty → generate; set → idempotent re-register
	Hostname     string
	AZ           string
	Rack         string // optional sub-AZ placement tag; see [[weft-placement-rules]]
	Endpoint     string
	Hypervisor   string
	Architecture string
	// Drivers is the full capability list — one entry per
	// weft-driver-<kind> subprocess the registering agent has
	// launched. Optional ; empty falls back to a single-driver
	// registration described by (Hypervisor, Architecture). See
	// [[Host.Drivers]] for the model.
	Drivers        []HostDriver
	NetworkTypes   []string
	VolumeBackends []string
	// GPUs is the host's physical accelerator inventory. Populated
	// by the agent at registration time via detectGPUs() (currently
	// a static stub — real detection is a follow-up documented in
	// docs/operations/gpu-scheduling.md). Operators staging GPU
	// hosts today can also seed this field statically via the
	// cluster.hcl `gpu { … }` blocks → registry path.
	GPUs []GPU
	// PCIDevices is the host's PCI passthrough inventory. Populated
	// by the agent via detectPCI() (today a Linux-only stub —
	// docs/operations/pci-passthrough.md walks the sysfs path) ;
	// operators can also seed it statically through cluster.hcl's
	// `pci { … }` blocks during the stub era.
	PCIDevices []PCIDevice
	Properties map[string]string
	// AKName is the TPM Attestation Key Name the registering node proved
	// at admission (set only on the attested path ; empty on the default
	// OIDC-only path). Stamped onto the Host registry entry. See
	// [[attestation.go]] for the gate.
	AKName string
	// WGPublicKey is the host's WireGuard public key for the host-level
	// overlay mesh. The registering node mints its keypair locally
	// (EnsureHostWGKey) and supplies ONLY the public half here — there is no
	// field for the private key by design, so a secret can never reach the
	// registry. Empty on the legacy / non-mesh registration path.
	WGPublicKey string
	// WGOverlayIndex is the host's stable 1-based overlay-subnet index (see
	// Host.WGOverlayIndex). Zero leaves the host out of the host mesh.
	WGOverlayIndex int
}

// register adds a new host or, when spec.UUID matches an
// existing entry, refreshes its mutable metadata (idempotent
// re-registration).
//
// Behaviour:
//
//   - spec.UUID == "" : mint a fresh UUID; reject if the
//     hostname is already taken by a different host.
//   - spec.UUID set, no existing host with that UUID : create
//     with the provided UUID; same hostname-uniqueness check.
//   - spec.UUID set, host already exists : refresh AZ, Endpoint,
//     Hypervisor, Architecture, NetworkTypes, VolumeBackends,
//     Properties; bump LastSeenAt; revive State if Down (operator
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
			// Placement metadata (AZ/Rack/Properties) follows the
			// don't-clobber-on-empty rule the AKName + WGPublicKey
			// branches below already use : empty spec means "keep
			// the prior value" so a manual `nohup weft agent &`
			// relaunch without WEFT_AZ/WEFT_RACK doesn't blank the
			// cluster.hcl-time placement. Operators explicitly moving
			// a host between racks pass non-empty values and those
			// still take precedence. Clearing the property set
			// explicitly goes through SetHostProperties with a
			// nil/empty map, not through a re-register.
			if spec.AZ != "" {
				existing.AZ = spec.AZ
			}
			if spec.Rack != "" {
				existing.Rack = spec.Rack
			}
			existing.Endpoint = spec.Endpoint
			existing.Hypervisor = spec.Hypervisor
			existing.Architecture = spec.Architecture
			existing.Drivers = cloneHostDrivers(spec.Drivers)
			existing.NetworkTypes = append([]string(nil), spec.NetworkTypes...)
			existing.VolumeBackends = append([]string(nil), spec.VolumeBackends...)
			existing.GPUs = cloneGPUs(spec.GPUs)
			existing.PCIDevices = clonePCIDevices(spec.PCIDevices)
			if len(spec.Properties) > 0 {
				existing.Properties = make(map[string]string, len(spec.Properties))
				for k, v := range spec.Properties {
					existing.Properties[k] = v
				}
			}
			existing.LastSeenAt = now
			if existing.State == HostStateDown {
				existing.State = HostStateActive
			}
			// Stamp the attested AK Name only when the caller supplies
			// one (attested path). The OIDC-only path leaves spec.AKName
			// empty, so a re-register through it preserves any prior value
			// rather than clobbering it.
			if spec.AKName != "" {
				existing.AKName = spec.AKName
			}
			// Host-mesh identity: refresh only when the caller supplies one,
			// mirroring the AKName "don't clobber on the legacy path" rule.
			// A node re-registering through the mesh path keeps its stable
			// pubkey + index ; a legacy re-register leaves any prior value.
			if spec.WGPublicKey != "" {
				existing.WGPublicKey = spec.WGPublicKey
			}
			if spec.WGOverlayIndex != 0 {
				existing.WGOverlayIndex = spec.WGOverlayIndex
			}
			r.byUUID[spec.UUID] = existing
			if err := r.persistOne(existing); err != nil {
				return Host{}, err
			}
			return existing, nil
		}
	}

	// Fresh registration. Hostname collisions normally reject — but a
	// same-hostname / different-UUID re-register is the expected pattern
	// when an operator re-images a host (state/ wiped, host-uuid file
	// regenerated under a new UUID). Accept the takeover when the prior
	// entry is provably stale : State==Down (dispatch-session sweeper
	// flipped it) OR LastSeenAt older than staleHostTakeoverAge (no
	// heartbeat seen). Otherwise refuse — a concurrent live host on the
	// same hostname is the only case we still want to surface as an
	// error, since silently hijacking a live agent's identity is unsafe.
	if priorUUID, taken := r.nameIdx[spec.Hostname]; taken {
		prior, ok := r.byUUID[priorUUID]
		stale := ok && (prior.State == HostStateDown || now.Sub(prior.LastSeenAt) > staleHostTakeoverAge)
		if !ok || !stale {
			return Host{}, fmt.Errorf("hostname %q already registered (UUID %s, last seen %s)", spec.Hostname, priorUUID, prior.LastSeenAt.UTC().Format(time.RFC3339))
		}
		// Preserve placement metadata (AZ, Rack, Properties) from the
		// prior entry when the new spec doesn't supply them. selfRegister
		// reads WEFT_AZ / WEFT_RACK from env — operators who relaunch
		// the agent via `nohup weft agent &` without exporting those
		// vars would otherwise lose the cluster.hcl-time placement on
		// every takeover. Empty spec fields = "keep the prior value" ;
		// non-empty = explicit override.
		if spec.AZ == "" {
			spec.AZ = prior.AZ
		}
		if spec.Rack == "" {
			spec.Rack = prior.Rack
		}
		if len(spec.Properties) == 0 && len(prior.Properties) > 0 {
			spec.Properties = make(map[string]string, len(prior.Properties))
			for k, v := range prior.Properties {
				spec.Properties[k] = v
			}
		}
		// Takeover : evict the stale entry before claiming the hostname.
		// On a persist failure we restore the in-memory state so a retry
		// can observe the same prior entry and re-attempt cleanly.
		delete(r.byUUID, priorUUID)
		delete(r.nameIdx, spec.Hostname)
		if err := r.persistDelete(priorUUID); err != nil {
			r.byUUID[priorUUID] = prior
			r.nameIdx[spec.Hostname] = priorUUID
			return Host{}, fmt.Errorf("hostname takeover: delete prior %s: %w", priorUUID, err)
		}
	}
	uuid := spec.UUID
	if uuid == "" {
		uuid = newUUID()
	}
	var properties map[string]string
	if len(spec.Properties) > 0 {
		properties = make(map[string]string, len(spec.Properties))
		for k, v := range spec.Properties {
			properties[k] = v
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
		Drivers:        cloneHostDrivers(spec.Drivers),
		NetworkTypes:   append([]string(nil), spec.NetworkTypes...),
		VolumeBackends: append([]string(nil), spec.VolumeBackends...),
		GPUs:           cloneGPUs(spec.GPUs),
		PCIDevices:     clonePCIDevices(spec.PCIDevices),
		Properties:     properties,
		State:          HostStateActive,
		LastSeenAt:     now,
		CreatedAt:      now,
		AKName:         spec.AKName,
		WGPublicKey:    spec.WGPublicKey,
		WGOverlayIndex: spec.WGOverlayIndex,
	}
	r.byUUID[h.UUID] = h
	r.nameIdx[h.Hostname] = h.UUID
	if err := r.persistOne(h); err != nil {
		delete(r.byUUID, h.UUID)
		delete(r.nameIdx, h.Hostname)
		return Host{}, err
	}
	return h, nil
}

// cloneHostDrivers deep-copies a Drivers slice so the registry can't
// be mutated through the caller's spec pointer. Nil-in / empty-in →
// nil-out, keeping the JSON-omitempty contract on the registry side.
func cloneHostDrivers(src []HostDriver) []HostDriver {
	if len(src) == 0 {
		return nil
	}
	out := make([]HostDriver, len(src))
	for i, d := range src {
		out[i] = HostDriver{
			Kind:   d.Kind,
			Arches: append([]string(nil), d.Arches...),
		}
	}
	return out
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
	return r.persistOne(h)
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
	return r.persistOne(h)
}

// setCordoned flips the Cordoned flag. Idempotent — no-op + nil
// when the host is already in the requested state (so the CLI
// can be safely called from re-entrant operator scripts).
//
// Independent of State : a Draining host that operators then
// uncordon stays Draining ; a Down host stays Down. Cordoned is
// purely a scheduler-visibility flag, not a lifecycle stage.
func (r *hostRegistry) setCordoned(uuid string, cordoned bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("host %q not found", uuid)
	}
	if h.Cordoned == cordoned {
		return nil
	}
	h.Cordoned = cordoned
	r.byUUID[uuid] = h
	return r.persistOne(h)
}

// setProperties replaces the properties map atomically.
func (r *hostRegistry) setProperties(uuid string, properties map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("host %q not found", uuid)
	}
	if len(properties) > 0 {
		copy := make(map[string]string, len(properties))
		for k, v := range properties {
			copy[k] = v
		}
		h.Properties = copy
	} else {
		h.Properties = nil
	}
	r.byUUID[uuid] = h
	return r.persistOne(h)
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
	return r.persistDelete(uuid)
}
