package weft

// vms.go owns the cluster's VM inventory: one entry per
// long-lived virtual machine the platform manages. This is the
// 8th Storage-backed registry in weft-control and the keystone
// of the multi-host story — every entry carries the `host_uuid`
// that resolves to a driver `HostHandle` in the dispatch table.
//
// Scope decision: VMs are **per-project**. Two projects can each
// have a VM named "web"; the (project_uuid, name) tuple disambiguates.
// Same multi-tenant pattern as networks/volumes/SG/ports.
//
// Schema:
//
//   vm "abc-…" {
//     project_uuid    = "p-…"
//     name            = "web-01"
//     host_uuid       = "h-…"           # chosen by scheduler at Create
//     image           = "ghcr.io/...:latest"
//     cpu_count       = 2
//     memory_mib      = 2048
//     state           = "running"       # created | running | stopped | deleting
//     created_at      = "..."
//     last_start_at   = "..."           # optional
//   }
//
// State machine:
//
//   created → running → stopped → deleting → (gone)
//                ↓ ↑
//   stopped ← running (graceful or crash)
//
// `created` covers the brief window after CloneVM provisions the
// vmDir but before StartVM. `running` is set by StartVM, cleared
// by StopVM. `deleting` is the soft-delete tombstone used to
// distinguish "user deleted, dispatcher should ignore" from
// "transient registry miss" — the actual removal happens in
// delete() after the driver's DeleteVM returns.
//
// Cross-registry validation (all in the Adapter wrapper):
//
//   * project_uuid must exist in projectRegistry.
//   * host_uuid must exist in hostRegistry AND in the driver
//     dispatch table (`HostHandleOn`).
//   * Capacity / capability matching is the scheduler's job —
//     the registry trusts the host_uuid passed in.

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

// VMState enumerates the lifecycle phases an operator + the
// scheduler care about. Kept short on purpose — finer-grained
// states (booting, draining, migrating) are layered as properties
// rather than first-class states to keep transition graphs
// small.
type VMState string

const (
	// VMStateCreated: vmDir provisioned, not yet started.
	VMStateCreated VMState = "created"
	// VMStateRunning: StartVM succeeded; vm.pid present.
	VMStateRunning VMState = "running"
	// VMStateZombie : the registry says the VM is running but the
	// actual process is gone — or the owning host has been Down past
	// its lease grace and respawn was skipped (typically a CI VM
	// covered by the deployment.type=ci respawn-skip gate). Set by
	// zombiegc.Reconciler ; cleared by either operator action or
	// auto-delete after a grace period (CI-only).
	VMStateZombie VMState = "zombie"
	// VMStateStopped: StopVM completed OR subprocess crashed.
	VMStateStopped VMState = "stopped"
	// VMStateDeleting: soft-delete in progress. Reconcilers
	// should treat this as "do not touch".
	VMStateDeleting VMState = "deleting"
)

// VM is one entry in the inventory.
type VM struct {
	UUID        string    `json:"uuid"`
	ProjectUUID string    `json:"project_uuid"`
	Name        string    `json:"name"`
	HostUUID    string    `json:"host_uuid"`
	Image       string    `json:"image,omitempty"`
	CPUCount    int       `json:"cpu_count,omitempty"`
	MemoryMiB   int       `json:"memory_mib,omitempty"`
	// Architecture is the guest's required CPU arch. Drives dispatch
	// on multi-driver hosts (Apple Silicon running VZ + QEMU
	// side-by-side : arm64 → vz, amd64 → qemu) via
	// Adapter.HostHandleOnArch. Empty = legacy behaviour, picks the
	// host's primary driver (matches single-driver clusters).
	Architecture string `json:"architecture,omitempty"`
	// RequestedGPUs is the fine-grained GPU shape this VM asked for
	// at creation time (vendor + model + count + optional MIG
	// slice). The scheduler consumes this for placement (see
	// scheduler.go's ScheduleRequest.RequestedGPUs) ; the
	// tenant-quotas layer reads it to sum the project-wide GPU
	// footprint when admitting a new VM — without this field the
	// aggregate cap can only catch a single oversized request, not
	// the (3 × 1-GPU VMs already running, 4th 1-GPU VM admitted
	// past a cap=3) drift.
	//
	// Persists through the HCL round-trip as one nested
	// `requested_gpu "<vendor>:<model>" { count = … mig_slice = … }`
	// block per entry. A nil/empty slice (the back-compat shape for
	// every VMInfo block written before this field landed) means
	// "this VM didn't request a GPU" — the aggregate skips it.
	RequestedGPUs []GPURequest `json:"requested_gpus,omitempty"`
	// RequestedPCI is the non-GPU PCI passthrough shape this VM
	// asked for at creation time (vendor:device:count). The
	// scheduler consumes the same shape via
	// ScheduleRequest.RequestedPCI ; the tenant-quotas layer reads
	// it here to sum the project-wide PCI footprint when admitting
	// a new VM, mirroring the RequestedGPUs aggregate path.
	//
	// Persists through the HCL round-trip as one nested
	// `requested_pci "<vendor>:<device>" { vendor_id = … device_id = …
	// count = … }` block per entry, mirroring the requested_gpu shape.
	// A nil/empty slice (back-compat for VMInfo blocks written before
	// this field landed) means "this VM didn't request a PCI
	// passthrough" — the aggregate skips it.
	RequestedPCI []PCIRequest `json:"requested_pci,omitempty"`
	// Properties is the operator-supplied k=v annotation set used by
	// SchedulingRule property-based selectors (V0.1.8). Example :
	// {"role":"loom","tier":"prod"}. Persists through HCL via a
	// properties = { ... } attribute. Empty / nil = no properties, selector
	// can only match by vm.name.
	Properties   map[string]string `json:"properties,omitempty"`
	State        VMState      `json:"state"`
	CreatedAt    time.Time    `json:"created_at"`
	LastStartAt  time.Time    `json:"last_start_at,omitempty"`
}

// vmsDoc / vmBlock mirror the HCL schema.
type vmsDoc struct {
	VMs []vmBlock `hcl:"vm,block"`
}

type vmBlock struct {
	UUID         string              `hcl:",label"`
	ProjectUUID  string              `hcl:"project_uuid"`
	Name         string              `hcl:"name"`
	HostUUID     string              `hcl:"host_uuid"`
	Image        string              `hcl:"image,optional"`
	CPUCount     int                 `hcl:"cpu_count,optional"`
	MemoryMiB    int                 `hcl:"memory_mib,optional"`
	Architecture string              `hcl:"architecture,optional"`
	RequestedGPU []requestedGPUBlock `hcl:"requested_gpu,block"`
	RequestedPCI []requestedPCIBlock `hcl:"requested_pci,block"`
	Properties   map[string]string   `hcl:"properties,optional"`
	State        string              `hcl:"state,optional"`
	CreatedAt    string              `hcl:"created_at"`
	LastStartAt  string              `hcl:"last_start_at,optional"`
}

// requestedGPUBlock is the HCL on-disk shape for one GPURequest
// entry attached to a VM. The label combines vendor + model so
// the operator can read the file by the requested SKU first ;
// count + MIG slice live inside the block. Mirrors hosts.go's
// gpuBlock pattern (host-side INVENTORY) — same encoding shape,
// different semantic axis (VM-side REQUEST).
type requestedGPUBlock struct {
	Label    string `hcl:",label"` // "vendor:model" — display key only
	Vendor   string `hcl:"vendor"`
	Model    string `hcl:"model,optional"`
	Count    int    `hcl:"count,optional"`
	MIGSlice string `hcl:"mig_slice,optional"`
}

// requestedPCIBlock is the HCL on-disk shape for one PCIRequest
// entry attached to a VM. The label combines vendor + device so
// the operator can read the file by the requested SKU first ;
// count lives inside the block. Mirrors requestedGPUBlock —
// same encoding pattern (label = "<vendor>:<device>"), different
// passthrough class (non-GPU PCI vs GPU).
type requestedPCIBlock struct {
	Label    string `hcl:",label"` // "vendor:device" — display key only
	VendorID string `hcl:"vendor_id"`
	DeviceID string `hcl:"device_id,optional"`
	Count    int    `hcl:"count,optional"`
}

// vmRegistry mirrors the multi-tenant registries (networks,
// volumes, security_groups, ports). Four indexes:
//
//   byUUID    — primary lookup, every public method goes through it
//   nameIdx   — (projectUUID,name) → UUID, scoped per project
//   projectIdx — projectUUID → set-of-UUIDs (list-by-project)
//   hostIdx   — hostUUID → set-of-UUIDs (list-by-host, used by the
//               future reconciler + by HostHandle disconnect cleanup)
type vmRegistry struct {
	mu         sync.Mutex
	storage    Storage   // blob-mode backend (legacy / file / mem)
	kv         KVStorage // per-record backend (V0.1.4 etcd KV) — nil if blob-mode
	byUUID     map[string]VM
	nameIdx    map[string]string              // (projectUUID,name) → UUID
	projectIdx map[string]map[string]struct{} // projectUUID → set
	hostIdx    map[string]map[string]struct{} // hostUUID → set
}

func loadVMRegistry(ctx context.Context, storage Storage) (*vmRegistry, error) {
	reg := &vmRegistry{
		storage:    storage,
		byUUID:     make(map[string]VM),
		nameIdx:    make(map[string]string),
		projectIdx: make(map[string]map[string]struct{}),
		hostIdx:    make(map[string]map[string]struct{}),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load vm registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc vmsDoc
	if err := hclsimple.Decode("vm-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse vm registry: %w", err)
	}
	for _, b := range doc.VMs {
		created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		var lastStart time.Time
		if b.LastStartAt != "" {
			lastStart, _ = time.Parse(time.RFC3339Nano, b.LastStartAt)
		}
		state := VMState(b.State)
		if state == "" {
			state = VMStateCreated
		}
		var reqGPUs []GPURequest
		if len(b.RequestedGPU) > 0 {
			reqGPUs = make([]GPURequest, 0, len(b.RequestedGPU))
			for _, rg := range b.RequestedGPU {
				reqGPUs = append(reqGPUs, GPURequest{
					Vendor:   rg.Vendor,
					Model:    rg.Model,
					Count:    rg.Count,
					MIGSlice: rg.MIGSlice,
				})
			}
		}
		var reqPCI []PCIRequest
		if len(b.RequestedPCI) > 0 {
			reqPCI = make([]PCIRequest, 0, len(b.RequestedPCI))
			for _, rp := range b.RequestedPCI {
				reqPCI = append(reqPCI, PCIRequest{
					VendorID: rp.VendorID,
					DeviceID: rp.DeviceID,
					Count:    rp.Count,
				})
			}
		}
		v := VM{
			UUID:          b.UUID,
			ProjectUUID:   b.ProjectUUID,
			Name:          b.Name,
			HostUUID:      b.HostUUID,
			Image:         b.Image,
			CPUCount:      b.CPUCount,
			MemoryMiB:     b.MemoryMiB,
			Architecture:  b.Architecture,
			RequestedGPUs: reqGPUs,
			RequestedPCI:  reqPCI,
			Properties:    copyProperties(b.Properties),
			State:         state,
			CreatedAt:     created,
			LastStartAt:   lastStart,
		}
		reg.indexLocked(v)
	}
	return reg, nil
}

func vmNameKey(projectUUID, name string) string {
	return projectUUID + "\x00" + name
}

// copyProperties returns a shallow copy of the properties map. Nil/empty
// in → nil out so the JSON-omitempty / HCL "properties = {}" round-trip
// stays clean. Callers should always go through this helper when
// reading b.Properties into a VM struct to avoid sharing the registry's
// map with a caller's pointer.
func copyProperties(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// indexLocked adds v to every secondary index. Caller holds mu
// (or is in the load path before publication).
func (r *vmRegistry) indexLocked(v VM) {
	r.byUUID[v.UUID] = v
	r.nameIdx[vmNameKey(v.ProjectUUID, v.Name)] = v.UUID
	if _, ok := r.projectIdx[v.ProjectUUID]; !ok {
		r.projectIdx[v.ProjectUUID] = make(map[string]struct{})
	}
	r.projectIdx[v.ProjectUUID][v.UUID] = struct{}{}
	if v.HostUUID != "" {
		if _, ok := r.hostIdx[v.HostUUID]; !ok {
			r.hostIdx[v.HostUUID] = make(map[string]struct{})
		}
		r.hostIdx[v.HostUUID][v.UUID] = struct{}{}
	}
}

// unindexLocked removes v from every secondary index. Caller
// holds mu.
func (r *vmRegistry) unindexLocked(v VM) {
	delete(r.byUUID, v.UUID)
	delete(r.nameIdx, vmNameKey(v.ProjectUUID, v.Name))
	delete(r.projectIdx[v.ProjectUUID], v.UUID)
	if len(r.projectIdx[v.ProjectUUID]) == 0 {
		delete(r.projectIdx, v.ProjectUUID)
	}
	if v.HostUUID != "" {
		delete(r.hostIdx[v.HostUUID], v.UUID)
		if len(r.hostIdx[v.HostUUID]) == 0 {
			delete(r.hostIdx, v.HostUUID)
		}
	}
}

// saveLocked writes the registry via Storage. Caller holds mu.
// HCL output is sorted by UUID for stable diffs ; the bytes live
// inside an etcd value (`etcdctl get vms`) so HCL stays the
// human-readable format we already have a parser for.
func (r *vmRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# weft VM inventory — UUID-keyed per [[weft-uuid-keyed-resources]].\n" +
				"# Each vm block carries its host_uuid (chosen by the scheduler at\n" +
				"# Create) so multi-host dispatch can route lifecycle calls.\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		v := r.byUUID[u]
		block := body.AppendNewBlock("vm", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("project_uuid", cty.StringVal(v.ProjectUUID))
		bb.SetAttributeValue("name", cty.StringVal(v.Name))
		bb.SetAttributeValue("host_uuid", cty.StringVal(v.HostUUID))
		if v.Image != "" {
			bb.SetAttributeValue("image", cty.StringVal(v.Image))
		}
		if v.CPUCount > 0 {
			bb.SetAttributeValue("cpu_count", cty.NumberIntVal(int64(v.CPUCount)))
		}
		if v.MemoryMiB > 0 {
			bb.SetAttributeValue("memory_mib", cty.NumberIntVal(int64(v.MemoryMiB)))
		}
		if v.Architecture != "" {
			bb.SetAttributeValue("architecture", cty.StringVal(v.Architecture))
		}
		for _, g := range v.RequestedGPUs {
			label := g.Vendor + ":" + g.Model
			gb := bb.AppendNewBlock("requested_gpu", []string{label}).Body()
			gb.SetAttributeValue("vendor", cty.StringVal(g.Vendor))
			if g.Model != "" {
				gb.SetAttributeValue("model", cty.StringVal(g.Model))
			}
			if g.Count > 0 {
				gb.SetAttributeValue("count", cty.NumberIntVal(int64(g.Count)))
			}
			if g.MIGSlice != "" {
				gb.SetAttributeValue("mig_slice", cty.StringVal(g.MIGSlice))
			}
		}
		for _, p := range v.RequestedPCI {
			label := p.VendorID + ":" + p.DeviceID
			pb := bb.AppendNewBlock("requested_pci", []string{label}).Body()
			pb.SetAttributeValue("vendor_id", cty.StringVal(p.VendorID))
			if p.DeviceID != "" {
				pb.SetAttributeValue("device_id", cty.StringVal(p.DeviceID))
			}
			if p.Count > 0 {
				pb.SetAttributeValue("count", cty.NumberIntVal(int64(p.Count)))
			}
		}
		if len(v.Properties) > 0 {
			ctyMap := make(map[string]cty.Value, len(v.Properties))
			for k, lv := range v.Properties {
				ctyMap[k] = cty.StringVal(lv)
			}
			bb.SetAttributeValue("properties", cty.MapVal(ctyMap))
		}
		if v.State != "" {
			bb.SetAttributeValue("state", cty.StringVal(string(v.State)))
		}
		bb.SetAttributeValue("created_at", cty.StringVal(v.CreatedAt.Format(time.RFC3339Nano)))
		if !v.LastStartAt.IsZero() {
			bb.SetAttributeValue("last_start_at", cty.StringVal(v.LastStartAt.Format(time.RFC3339Nano)))
		}
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

func validateVMState(s VMState) error {
	switch s {
	case "", VMStateCreated, VMStateRunning, VMStateStopped, VMStateDeleting, VMStateZombie:
		return nil
	default:
		return fmt.Errorf("unknown vm state %q (want created, running, stopped, or deleting)", s)
	}
}

// lookupByUUID returns the VM with the given UUID.
func (r *vmRegistry) lookupByUUID(uuid string) (VM, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	return v, ok
}

// lookupByName resolves (projectUUID, name) to a VM. Per-project
// scope means two projects can have a "web" each.
func (r *vmRegistry) lookupByName(projectUUID, name string) (VM, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[vmNameKey(projectUUID, name)]
	if !ok {
		return VM{}, false
	}
	v, ok := r.byUUID[uuid]
	return v, ok
}

// listForProject returns every VM in the project, sorted by name.
func (r *vmRegistry) listForProject(projectUUID string) []VM {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.projectIdx[projectUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]VM, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// listForHost returns every VM on the host. Used by the future
// reconciler when an agent disconnects: "which VMs lost their
// driver?" → mark them stopped, schedule failover.
func (r *vmRegistry) listForHost(hostUUID string) []VM {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.hostIdx[hostUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]VM, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// reloadFromStorage re-reads the registry blob from Storage and
// swaps the in-memory maps under the lock. Called by the agent's
// etcd watcher loop when a remote agent (another DC) writes the
// shared `vms` key — without this, the local hostIdx stays stuck
// on the snapshot read at boot, and a cross-host failover claim
// can't find orphan VMs whose host_uuid was just flipped to a
// dead DC by RegisterMicroVM-claim.
//
// Errors here are logged by the caller but never crash the agent ;
// a transient parse failure leaves the previous snapshot in place.
func (r *vmRegistry) reloadFromStorage(ctx context.Context) error {
	blob, err := r.storage.Load(ctx)
	if err != nil {
		return fmt.Errorf("reload vm registry: %w", err)
	}
	byUUID := make(map[string]VM)
	nameIdx := make(map[string]string)
	projectIdx := make(map[string]map[string]struct{})
	hostIdx := make(map[string]map[string]struct{})
	if len(blob) > 0 {
		var doc vmsDoc
		if err := hclsimple.Decode("vm-registry.hcl", blob, nil, &doc); err != nil {
			return fmt.Errorf("reload parse vm registry: %w", err)
		}
		for _, b := range doc.VMs {
			created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
			var lastStart time.Time
			if b.LastStartAt != "" {
				lastStart, _ = time.Parse(time.RFC3339Nano, b.LastStartAt)
			}
			state := VMState(b.State)
			if state == "" {
				state = VMStateCreated
			}
			var reqGPUs []GPURequest
			for _, rg := range b.RequestedGPU {
				reqGPUs = append(reqGPUs, GPURequest{
					Vendor: rg.Vendor, Model: rg.Model,
					Count: rg.Count, MIGSlice: rg.MIGSlice,
				})
			}
			var reqPCI []PCIRequest
			for _, rp := range b.RequestedPCI {
				reqPCI = append(reqPCI, PCIRequest{
					VendorID: rp.VendorID, DeviceID: rp.DeviceID, Count: rp.Count,
				})
			}
			v := VM{
				UUID:          b.UUID,
				ProjectUUID:   b.ProjectUUID,
				Name:          b.Name,
				HostUUID:      b.HostUUID,
				Image:         b.Image,
				CPUCount:      b.CPUCount,
				MemoryMiB:     b.MemoryMiB,
				Architecture:  b.Architecture,
				RequestedGPUs: reqGPUs,
				RequestedPCI:  reqPCI,
				Properties:    copyProperties(b.Properties),
				State:         state,
				CreatedAt:     created,
				LastStartAt:   lastStart,
			}
			byUUID[v.UUID] = v
			nameIdx[vmNameKey(v.ProjectUUID, v.Name)] = v.UUID
			if _, ok := projectIdx[v.ProjectUUID]; !ok {
				projectIdx[v.ProjectUUID] = make(map[string]struct{})
			}
			projectIdx[v.ProjectUUID][v.UUID] = struct{}{}
			if v.HostUUID != "" {
				if _, ok := hostIdx[v.HostUUID]; !ok {
					hostIdx[v.HostUUID] = make(map[string]struct{})
				}
				hostIdx[v.HostUUID][v.UUID] = struct{}{}
			}
		}
	}
	r.mu.Lock()
	r.byUUID = byUUID
	r.nameIdx = nameIdx
	r.projectIdx = projectIdx
	r.hostIdx = hostIdx
	r.mu.Unlock()
	return nil
}

// list returns every VM across all projects, sorted by
// (ProjectUUID, Name).
func (r *vmRegistry) list() []VM {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]VM, 0, len(r.byUUID))
	for _, v := range r.byUUID {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CreateVMSpec carries the inputs for create. ProjectUUID,
// Name, HostUUID are required. Image / CPUCount / MemoryMiB are
// optional metadata (the canonical source for what's actually
// running is the host's `config.json` + driver state).
type CreateVMSpec struct {
	ProjectUUID string
	Name        string
	HostUUID    string
	Image       string
	CPUCount    int
	MemoryMiB   int
	// Architecture is the guest CPU arch ("arm64" | "amd64" |
	// "riscv64" | "loongarch64"). Empty = inherit the host's
	// native arch (back-compat with single-driver clusters where
	// every VM ran the host's arch). Drives dispatch via
	// Adapter.HostHandleOnArch on multi-driver hosts.
	Architecture string
	// RequestedGPUs is the fine-grained GPU request the VM asked
	// for. Recorded on the VM at registration time so the
	// tenant-quotas layer can sum the project-wide footprint
	// across all VMs (closes the per-request-only gap left by
	// commit 3f18e2a2d). Nil/empty = no GPU request.
	RequestedGPUs []GPURequest
	// RequestedPCI is the non-GPU PCI passthrough request the VM
	// asked for. Recorded on the VM at registration time so the
	// tenant-quotas layer can sum the project-wide pci_count
	// across all VMs — same aggregate path as RequestedGPUs.
	// Nil/empty = no PCI request.
	RequestedPCI []PCIRequest
	// Properties is the V0.1.8 operator-supplied annotation set.
	// Persisted as an HCL `properties = { … }` attribute ; consumed by
	// SchedulingRule property-based selectors (e.g. `role=loom`).
	// Nil/empty = no properties (selector can only match by vm.name).
	Properties map[string]string
}

// create registers a new VM. Refuses name collisions within the
// same project + empty required fields. Cross-registry validation
// (project_uuid / host_uuid existence) happens at the Adapter
// wrapper level.
func (r *vmRegistry) create(spec CreateVMSpec) (VM, error) {
	if spec.ProjectUUID == "" {
		return VM{}, fmt.Errorf("empty project_uuid")
	}
	if spec.Name == "" {
		return VM{}, fmt.Errorf("empty vm name")
	}
	if spec.HostUUID == "" {
		return VM{}, fmt.Errorf("empty host_uuid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.nameIdx[vmNameKey(spec.ProjectUUID, spec.Name)]; taken {
		return VM{}, fmt.Errorf("vm name %q already in use in project %s", spec.Name, spec.ProjectUUID)
	}
	// Deep-copy RequestedGPUs so the registry can't be mutated
	// through the caller's spec slice. Same pattern as cloneGPUs
	// in gpu.go : nil/empty in → nil out (HCL omit-when-empty +
	// JSON omitempty contracts stay clean).
	var reqGPUs []GPURequest
	if len(spec.RequestedGPUs) > 0 {
		reqGPUs = make([]GPURequest, len(spec.RequestedGPUs))
		copy(reqGPUs, spec.RequestedGPUs)
	}
	// Deep-copy RequestedPCI for the same reason.
	var reqPCI []PCIRequest
	if len(spec.RequestedPCI) > 0 {
		reqPCI = make([]PCIRequest, len(spec.RequestedPCI))
		copy(reqPCI, spec.RequestedPCI)
	}
	v := VM{
		UUID:          newUUID(),
		ProjectUUID:   spec.ProjectUUID,
		Name:          spec.Name,
		HostUUID:      spec.HostUUID,
		Image:         spec.Image,
		CPUCount:      spec.CPUCount,
		MemoryMiB:     spec.MemoryMiB,
		Architecture:  spec.Architecture,
		RequestedGPUs: reqGPUs,
		RequestedPCI:  reqPCI,
		Properties:    copyProperties(spec.Properties),
		State:         VMStateCreated,
		CreatedAt:     time.Now().UTC(),
	}
	r.indexLocked(v)
	if err := r.persistOne(v); err != nil {
		r.unindexLocked(v)
		return VM{}, err
	}
	return v, nil
}

// setState transitions the VM. Validates the target state;
// transition graph is otherwise unconstrained — the reconciler
// + Adapter wrappers enforce ordering. Sets LastStartAt when
// moving into Running.
func (r *vmRegistry) setState(uuid string, state VMState) error {
	if err := validateVMState(state); err != nil {
		return err
	}
	if state == "" {
		return fmt.Errorf("empty state")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("vm %q not found", uuid)
	}
	if v.State == state {
		return nil
	}
	v.State = state
	if state == VMStateRunning {
		v.LastStartAt = time.Now().UTC()
	}
	r.byUUID[uuid] = v
	return r.persistOne(v)
}

// setHost migrates a VM to a different host. The VM-level move
// is a metadata flip; the actual data migration (disk image
// transfer, network handover, mesh-peer rotation) is the
// caller's responsibility — typically a future `weft vm migrate`
// command that orchestrates the steps.
func (r *vmRegistry) setHost(uuid, newHostUUID string) error {
	if newHostUUID == "" {
		return fmt.Errorf("empty host_uuid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("vm %q not found", uuid)
	}
	if v.HostUUID == newHostUUID {
		return nil
	}
	// Move between hostIdx entries.
	if v.HostUUID != "" {
		delete(r.hostIdx[v.HostUUID], uuid)
		if len(r.hostIdx[v.HostUUID]) == 0 {
			delete(r.hostIdx, v.HostUUID)
		}
	}
	if _, ok := r.hostIdx[newHostUUID]; !ok {
		r.hostIdx[newHostUUID] = make(map[string]struct{})
	}
	r.hostIdx[newHostUUID][uuid] = struct{}{}
	v.HostUUID = newHostUUID
	r.byUUID[uuid] = v
	return r.persistOne(v)
}

// setName renames within the project. Refuses collisions.
func (r *vmRegistry) setName(uuid, newName string) error {
	if newName == "" {
		return fmt.Errorf("empty vm name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("vm %q not found", uuid)
	}
	if v.Name == newName {
		return nil
	}
	newKey := vmNameKey(v.ProjectUUID, newName)
	if existing, taken := r.nameIdx[newKey]; taken && existing != uuid {
		return fmt.Errorf("vm name %q already in use in project %s", newName, v.ProjectUUID)
	}
	delete(r.nameIdx, vmNameKey(v.ProjectUUID, v.Name))
	v.Name = newName
	r.byUUID[uuid] = v
	r.nameIdx[newKey] = uuid
	return r.persistOne(v)
}

// setProperties replaces the property set atomically. nil/empty in →
// property set is cleared. V0.1.8 mutator for SchedulingRule property-
// based selectors ; the index doesn't carry properties (there's no
// propertyIdx because selectors are evaluated at request time, not
// pre-indexed) so this is a pure record update + persistOne.
func (r *vmRegistry) setProperties(uuid string, properties map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("vm %q not found", uuid)
	}
	v.Properties = copyProperties(properties)
	r.byUUID[uuid] = v
	return r.persistOne(v)
}

// delete drops a VM from the registry. Idempotent on missing
// UUIDs is NOT the contract — callers know what they're
// deleting + a "vm %q not found" error is more useful than
// silent success.
func (r *vmRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("vm %q not found", uuid)
	}
	r.unindexLocked(v)
	return r.persistDelete(uuid)
}
