package weft

// tenant_quotas.go is the hard-cap enforcement layer between the
// VM/Volume create handlers and the per-tenant catalogue. Today
// the only "tenant" granularity surfaced by the agent registry
// is the Project (per [[weft-uuid-keyed-resources]] — the
// `weft_tenant` resource webui ships against doesn't yet have a
// counterpart at the wire level). Quotas are therefore keyed on
// project UUID ; when the tenant model lands the cap can move
// up one level without touching the enforcement call sites.
//
// Per docs/operations/tenant-quotas.md the three dimensions we
// enforce at the agent today are :
//
//   * cpu_count  : Σ over the project's VMs of CPUCount
//   * memory_gib : Σ over the project's VMs of ceil(MemoryMiB/1024)
//   * volume_gib : Σ over the project's volumes of SizeGiB
//
// Unset (zero) cap means "no limit" — a fresh project is
// effectively unlimited until an operator sets caps via
// SetTenantQuota. The registry is loaded once at startup,
// rewritten on every mutation, persisted through the standard
// RegistryStorage("tenant-quotas") factory so the file / etcd
// backend choice transparently applies.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TenantQuota is the hard cap for one project (a.k.a. tenant at
// today's granularity ; see top-of-file). Zero values mean "no
// cap on this dimension" — operators set what they want to
// constrain and leave the rest unbounded.
type TenantQuota struct {
	CPUCount  int `json:"cpu_count,omitempty"`
	MemoryGiB int `json:"memory_gib,omitempty"`
	VolumeGiB int `json:"volume_gib,omitempty"`
}

// tenantQuotaDoc is the HCL on-disk shape. One `tenant_quota
// "<project-uuid>"` block per project ; missing dimensions =
// no cap on that dimension.
type tenantQuotaDoc struct {
	Quotas []tenantQuotaBlock `hcl:"tenant_quota,block"`
}

type tenantQuotaBlock struct {
	ProjectUUID string `hcl:",label"`
	CPUCount    int    `hcl:"cpu_count,optional"`
	MemoryGiB   int    `hcl:"memory_gib,optional"`
	VolumeGiB   int    `hcl:"volume_gib,optional"`
}

// tenantQuotaRegistry is the in-memory cache of the on-disk
// per-project caps. Same Storage-backed pattern as the rest of
// the registries (file / etcd / mem) ; see storage.go.
type tenantQuotaRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]TenantQuota
}

func loadTenantQuotaRegistry(ctx context.Context, storage Storage) (*tenantQuotaRegistry, error) {
	reg := &tenantQuotaRegistry{
		storage: storage,
		byUUID:  make(map[string]TenantQuota),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load tenant-quota registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc tenantQuotaDoc
	if err := hclsimple.Decode("tenant-quotas.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse tenant-quota registry: %w", err)
	}
	for _, b := range doc.Quotas {
		reg.byUUID[b.ProjectUUID] = TenantQuota{
			CPUCount:  b.CPUCount,
			MemoryGiB: b.MemoryGiB,
			VolumeGiB: b.VolumeGiB,
		}
	}
	return reg, nil
}

// saveLocked rewrites the registry through Storage. Caller holds mu.
// Blocks are emitted in UUID-sorted order so diffs stay stable.
func (r *tenantQuotaRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		q := r.byUUID[u]
		block := body.AppendNewBlock("tenant_quota", []string{u})
		bb := block.Body()
		if q.CPUCount > 0 {
			bb.SetAttributeValue("cpu_count", cty.NumberIntVal(int64(q.CPUCount)))
		}
		if q.MemoryGiB > 0 {
			bb.SetAttributeValue("memory_gib", cty.NumberIntVal(int64(q.MemoryGiB)))
		}
		if q.VolumeGiB > 0 {
			bb.SetAttributeValue("volume_gib", cty.NumberIntVal(int64(q.VolumeGiB)))
		}
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

func (r *tenantQuotaRegistry) get(projectUUID string) TenantQuota {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byUUID[projectUUID]
}

func (r *tenantQuotaRegistry) set(projectUUID string, q TenantQuota) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, had := r.byUUID[projectUUID]
	// Zero-only quota = clear. Treats `--cpu=0 --mem=0 --volume=0`
	// as "remove the entry" so operators can wipe a cap without
	// thinking about whether to call a separate Delete.
	if q.CPUCount == 0 && q.MemoryGiB == 0 && q.VolumeGiB == 0 {
		delete(r.byUUID, projectUUID)
	} else {
		r.byUUID[projectUUID] = q
	}
	if err := r.saveLocked(); err != nil {
		// Roll back the in-memory mutation so the cache matches what's
		// on disk after a partial failure.
		if had {
			r.byUUID[projectUUID] = prev
		} else {
			delete(r.byUUID, projectUUID)
		}
		return err
	}
	return nil
}

// SetTenantQuota assigns hard caps to the given project. Pass a
// zero TenantQuota to clear all caps for that project (the
// registry treats it as "remove the entry").
//
// Returns an error only on Storage failure ; passing an unknown
// project UUID is intentionally permitted — operators provision
// caps ahead of project creation.
func (a *Adapter) SetTenantQuota(projectUUID string, q TenantQuota) error {
	if a.tenantQuotas == nil {
		return fmt.Errorf("tenant-quota registry not initialised")
	}
	return a.tenantQuotas.set(projectUUID, q)
}

// TenantQuota returns the configured caps for the project. Zero
// fields mean "no limit on this dimension". Returns the zero
// value for an unconfigured project (effectively unlimited).
func (a *Adapter) TenantQuota(projectUUID string) TenantQuota {
	if a.tenantQuotas == nil {
		return TenantQuota{}
	}
	return a.tenantQuotas.get(projectUUID)
}

// projectAllocation sums the currently-allocated cpu/memory/volume
// across all VMs and volumes belonging to `projectUUID`. Used by
// the enforcement helpers to compare proposed (cap, allocated+new)
// pairs. Returned values are in units that match TenantQuota
// (CPUs as count, memory in GiB rounded up, volumes in GiB).
func (a *Adapter) projectAllocation(projectUUID string) TenantQuota {
	out := TenantQuota{}
	for _, v := range a.vmReg.listForProject(projectUUID) {
		out.CPUCount += v.CPUCount
		if v.MemoryMiB > 0 {
			out.MemoryGiB += ceilDivInt(v.MemoryMiB, 1024)
		}
	}
	for _, vol := range a.volumeReg.listForProject(projectUUID) {
		out.VolumeGiB += vol.SizeGiB
	}
	return out
}

// ceilDivInt is a min-overhead ceil(a/b) for non-negative ints.
// We use it to convert MiB → GiB so 2049 MiB counts as 3 GiB
// against the cap, not 2 — the cap describes the operator's
// commitment, undercounting would let tenants drift past it.
func ceilDivInt(a, b int) int {
	if b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// EnforceTenantQuotaForVM returns a gRPC ResourceExhausted error
// when admitting a VM with `cpu` vCPUs + `memoryMiB` of RAM would
// push the project's allocation past its cap. Zero cap on a
// dimension means "unlimited on that dimension" — the check
// short-circuits per axis.
func (a *Adapter) EnforceTenantQuotaForVM(projectUUID string, cpu, memoryMiB int) error {
	cap := a.TenantQuota(projectUUID)
	alloc := a.projectAllocation(projectUUID)
	newMemGiB := 0
	if memoryMiB > 0 {
		newMemGiB = ceilDivInt(memoryMiB, 1024)
	}
	if cap.CPUCount > 0 && alloc.CPUCount+cpu > cap.CPUCount {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: cpu (allocated %d + requested %d > cap %d)",
			alloc.CPUCount, cpu, cap.CPUCount)
	}
	if cap.MemoryGiB > 0 && alloc.MemoryGiB+newMemGiB > cap.MemoryGiB {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: memory_gib (allocated %d + requested %d > cap %d)",
			alloc.MemoryGiB, newMemGiB, cap.MemoryGiB)
	}
	return nil
}

// initTenantQuotas loads the on-disk tenant-quota registry via
// storageFactory. Mirrors the resilience contract used by
// initProjects / initVolumes : a load failure downgrades to an
// empty in-memory registry so the agent boots even if the file is
// corrupt — operators see the error on stderr and can correct it
// without weft refusing to start.
func (a *Adapter) initTenantQuotas() {
	storage := a.storageFactory("tenant-quotas")
	reg, err := loadTenantQuotaRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load tenant-quota registry: %v\n", err)
		reg = &tenantQuotaRegistry{
			storage: storage,
			byUUID:  make(map[string]TenantQuota),
		}
	}
	a.tenantQuotas = reg
}

// EnforceTenantQuotaForVolume returns ResourceExhausted when
// creating a volume of `sizeGiB` would push the project past its
// volume_gib cap. Zero cap is "no limit".
func (a *Adapter) EnforceTenantQuotaForVolume(projectUUID string, sizeGiB int) error {
	cap := a.TenantQuota(projectUUID)
	if cap.VolumeGiB <= 0 {
		return nil
	}
	alloc := a.projectAllocation(projectUUID)
	if alloc.VolumeGiB+sizeGiB > cap.VolumeGiB {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: volume_gib (allocated %d + requested %d > cap %d)",
			alloc.VolumeGiB, sizeGiB, cap.VolumeGiB)
	}
	return nil
}
