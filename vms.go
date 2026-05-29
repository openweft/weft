package weft

// vms.go owns the cluster's VM inventory: one entry per
// long-lived virtual machine the platform manages. This is the
// 8th Storage-backed registry in vzd-control and the keystone
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
// states (booting, draining, migrating) are layered as labels
// rather than first-class states to keep transition graphs
// small.
type VMState string

const (
	// VMStateCreated: vmDir provisioned, not yet started.
	VMStateCreated VMState = "created"
	// VMStateRunning: StartVM succeeded; vm.pid present.
	VMStateRunning VMState = "running"
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
	State       VMState   `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	LastStartAt time.Time `json:"last_start_at,omitempty"`
}

// vmsDoc / vmBlock mirror the HCL schema.
type vmsDoc struct {
	VMs []vmBlock `hcl:"vm,block"`
}

type vmBlock struct {
	UUID        string `hcl:",label"`
	ProjectUUID string `hcl:"project_uuid"`
	Name        string `hcl:"name"`
	HostUUID    string `hcl:"host_uuid"`
	Image       string `hcl:"image,optional"`
	CPUCount    int    `hcl:"cpu_count,optional"`
	MemoryMiB   int    `hcl:"memory_mib,optional"`
	State       string `hcl:"state,optional"`
	CreatedAt   string `hcl:"created_at"`
	LastStartAt string `hcl:"last_start_at,optional"`
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
	storage    Storage
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
		v := VM{
			UUID:        b.UUID,
			ProjectUUID: b.ProjectUUID,
			Name:        b.Name,
			HostUUID:    b.HostUUID,
			Image:       b.Image,
			CPUCount:    b.CPUCount,
			MemoryMiB:   b.MemoryMiB,
			State:       state,
			CreatedAt:   created,
			LastStartAt: lastStart,
		}
		reg.indexLocked(v)
	}
	return reg, nil
}

func vmNameKey(projectUUID, name string) string {
	return projectUUID + "\x00" + name
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
// HCL output is sorted by UUID for stable diffs.
func (r *vmRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# vzd VM inventory — UUID-keyed per [[vzd-uuid-keyed-resources]].\n" +
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
	case "", VMStateCreated, VMStateRunning, VMStateStopped, VMStateDeleting:
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
	v := VM{
		UUID:        newUUID(),
		ProjectUUID: spec.ProjectUUID,
		Name:        spec.Name,
		HostUUID:    spec.HostUUID,
		Image:       spec.Image,
		CPUCount:    spec.CPUCount,
		MemoryMiB:   spec.MemoryMiB,
		State:       VMStateCreated,
		CreatedAt:   time.Now().UTC(),
	}
	r.indexLocked(v)
	if err := r.saveLocked(); err != nil {
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
	return r.saveLocked()
}

// setHost migrates a VM to a different host. The VM-level move
// is a metadata flip; the actual data migration (disk image
// transfer, network handover, mesh-peer rotation) is the
// caller's responsibility — typically a future `vzc vm migrate`
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
	return r.saveLocked()
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
	return r.saveLocked()
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
	return r.saveLocked()
}
