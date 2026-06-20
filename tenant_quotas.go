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
	CPUCount     int `json:"cpu_count,omitempty"`
	MemoryGiB    int `json:"memory_gib,omitempty"`
	VolumeGiB    int `json:"volume_gib,omitempty"`
	GPUCount     int `json:"gpu_count,omitempty"`
	GPUMemoryGiB int `json:"gpu_memory_gib,omitempty"`
	// PCICount caps the aggregate non-GPU PCI passthrough count
	// across the project's VMs (NICs, NVMe, sound cards, FPGAs ;
	// GPUs go through GPUCount / GPUMemoryGiB instead). PCI
	// devices don't carry an aggregate-meaningful memory
	// dimension — NIC line-rate isn't sum-able to one cap and
	// NVMe capacity belongs under VolumeGiB once the device is
	// fronted as a volume — so the surface is a single
	// cardinality dimension, not the count + memory pair GPUs
	// got.
	PCICount int `json:"pci_count,omitempty"`

	// Proto-aligned dimensions (weftv1.Quotas v0.12.0+). Storage +
	// round-trip via Get/SetProjectQuota are wired today ;
	// enforcement at CreateVolume / CreateShare / CreateBucket /
	// AllocateFloatingIP is a follow-up that needs each registry's
	// projectAllocation helper to grow a count/size sum.
	VolumeCount int `json:"volume_count,omitempty"` // proto: volumes
	ShareCount  int `json:"share_count,omitempty"`  // proto: shares
	ShareGiB    int `json:"share_gib,omitempty"`    // proto: shares_gib
	BucketCount int `json:"bucket_count,omitempty"` // proto: buckets
	BucketGiB   int `json:"bucket_gib,omitempty"`   // proto: buckets_gib
	RegistryGiB int `json:"registry_gib,omitempty"` // proto: registry_gib
	FloatingIPs int `json:"floating_ips,omitempty"` // proto: floating_ips
}

// tenantQuotaDoc is the HCL on-disk shape. One `tenant_quota
// "<project-uuid>"` block per project ; missing dimensions =
// no cap on that dimension.
type tenantQuotaDoc struct {
	Quotas []tenantQuotaBlock `hcl:"tenant_quota,block"`
}

type tenantQuotaBlock struct {
	ProjectUUID  string `hcl:",label"`
	CPUCount     int    `hcl:"cpu_count,optional"`
	MemoryGiB    int    `hcl:"memory_gib,optional"`
	VolumeGiB    int    `hcl:"volume_gib,optional"`
	GPUCount     int    `hcl:"gpu_count,optional"`
	GPUMemoryGiB int    `hcl:"gpu_memory_gib,optional"`
	PCICount     int    `hcl:"pci_count,optional"`
	VolumeCount  int    `hcl:"volume_count,optional"`
	ShareCount   int    `hcl:"share_count,optional"`
	ShareGiB     int    `hcl:"share_gib,optional"`
	BucketCount  int    `hcl:"bucket_count,optional"`
	BucketGiB    int    `hcl:"bucket_gib,optional"`
	RegistryGiB  int    `hcl:"registry_gib,optional"`
	FloatingIPs  int    `hcl:"floating_ips,optional"`
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
			CPUCount:     b.CPUCount,
			MemoryGiB:    b.MemoryGiB,
			VolumeGiB:    b.VolumeGiB,
			GPUCount:     b.GPUCount,
			GPUMemoryGiB: b.GPUMemoryGiB,
			PCICount:     b.PCICount,
			VolumeCount:  b.VolumeCount,
			ShareCount:   b.ShareCount,
			ShareGiB:     b.ShareGiB,
			BucketCount:  b.BucketCount,
			BucketGiB:    b.BucketGiB,
			RegistryGiB:  b.RegistryGiB,
			FloatingIPs:  b.FloatingIPs,
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
		if q.GPUCount > 0 {
			bb.SetAttributeValue("gpu_count", cty.NumberIntVal(int64(q.GPUCount)))
		}
		if q.GPUMemoryGiB > 0 {
			bb.SetAttributeValue("gpu_memory_gib", cty.NumberIntVal(int64(q.GPUMemoryGiB)))
		}
		if q.PCICount > 0 {
			bb.SetAttributeValue("pci_count", cty.NumberIntVal(int64(q.PCICount)))
		}
		if q.VolumeCount > 0 {
			bb.SetAttributeValue("volume_count", cty.NumberIntVal(int64(q.VolumeCount)))
		}
		if q.ShareCount > 0 {
			bb.SetAttributeValue("share_count", cty.NumberIntVal(int64(q.ShareCount)))
		}
		if q.ShareGiB > 0 {
			bb.SetAttributeValue("share_gib", cty.NumberIntVal(int64(q.ShareGiB)))
		}
		if q.BucketCount > 0 {
			bb.SetAttributeValue("bucket_count", cty.NumberIntVal(int64(q.BucketCount)))
		}
		if q.BucketGiB > 0 {
			bb.SetAttributeValue("bucket_gib", cty.NumberIntVal(int64(q.BucketGiB)))
		}
		if q.RegistryGiB > 0 {
			bb.SetAttributeValue("registry_gib", cty.NumberIntVal(int64(q.RegistryGiB)))
		}
		if q.FloatingIPs > 0 {
			bb.SetAttributeValue("floating_ips", cty.NumberIntVal(int64(q.FloatingIPs)))
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
	// Zero-only quota = clear. Treats `--cpu=0 --mem=0 --volume=0
	// --volumes=0 --shares=0 ...` as "remove the entry" so operators
	// can wipe a cap without thinking about whether to call a separate
	// Delete. Uses isZeroQuota (tenant_caps.go) so any new TenantQuota
	// dimension automatically participates in the check.
	if isZeroQuota(q) {
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

// gpuModelMemoryGiB maps a canonical GPU model to its per-card
// memory in GiB. Used by projectAllocation to sum a project's
// gpu_memory_gib footprint across every VM's RequestedGPUs entry.
//
// Per [[openweft_gpu_fleet]] the supported fleet is **H200 +
// RTX 6000 Ada only** — other SKUs (L40S / H100 / A100) are
// intentionally absent. Unknown models contribute 0 to the
// memory sum : the operator can still cap by gpu_count, and a
// future host-inventory-driven lookup will replace this static
// map when the scheduler can source memory from the agent's
// detected GPUs instead of a hardcoded table.
//
// weft-internal, replace with a host-inventory-driven lookup
// when the scheduler sources memory from host info instead of
// hardcoded.
var gpuModelMemoryGiB = map[string]int{
	"H200":         141, // HBM3e
	"RTX-6000-Ada": 48,  // GDDR6
}

// gpuRequestMemoryGiB returns the memory contribution of one
// GPURequest entry : Count × per-card memory for known models,
// 0 for unknown SKUs (count cap still enforces the request).
// Case-insensitive match on Model so operators staging "h200"
// vs "H200" both land in the lookup.
func gpuRequestMemoryGiB(r GPURequest) int {
	count := r.Count
	if count <= 0 {
		count = 1
	}
	for model, mem := range gpuModelMemoryGiB {
		if equalFoldASCII(r.Model, model) {
			return count * mem
		}
	}
	return 0
}

// equalFoldASCII is a tiny ASCII-only case-fold compare. Kept
// local so this file doesn't grow a `strings` import for one
// callsite ; the canonical GPU model set is ASCII anyway.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// projectAllocation sums the currently-allocated cpu/memory/volume
// /gpu_count/gpu_memory_gib across all VMs and volumes belonging to
// `projectUUID`. Used by the enforcement helpers to compare proposed
// (cap, allocated+new) pairs. Returned values are in units that
// match TenantQuota (CPUs as count, memory in GiB rounded up, volumes
// in GiB, gpu_count as the per-VM sum of `len(RequestedGPUs)`-style
// requests with Count defaulted to 1, gpu_memory_gib via the
// gpuModelMemoryGiB table).
func (a *Adapter) projectAllocation(projectUUID string) TenantQuota {
	out := TenantQuota{}
	for _, v := range a.vmReg.listForProject(projectUUID) {
		out.CPUCount += v.CPUCount
		if v.MemoryMiB > 0 {
			out.MemoryGiB += ceilDivInt(v.MemoryMiB, 1024)
		}
		for _, g := range v.RequestedGPUs {
			c := g.Count
			if c <= 0 {
				c = 1
			}
			out.GPUCount += c
			out.GPUMemoryGiB += gpuRequestMemoryGiB(g)
		}
		for _, p := range v.RequestedPCI {
			c := p.Count
			if c <= 0 {
				c = 1
			}
			out.PCICount += c
		}
	}
	for _, vol := range a.volumeReg.listForProject(projectUUID) {
		out.VolumeGiB += vol.SizeGiB
		out.VolumeCount++
	}
	if a.shareReg != nil {
		for _, sh := range a.shareReg.list(projectUUID) {
			out.ShareCount++
			// Share.SizeGB is a misnomer ; the proto + operator
			// surface treat it as GiB-equivalent (the unit-name
			// drift predates the proto-aligned cap dimensions).
			// Aggregate as-is so the cap-vs-allocated comparison
			// stays consistent in the operator's mental model.
			out.ShareGiB += int(sh.SizeGB)
		}
	}
	if a.bucketReg != nil {
		for range a.bucketReg.list(projectUUID) {
			out.BucketCount++
		}
		// BucketGiB stays 0 : the local Bucket struct doesn't
		// carry a size field today (S3 buckets are catalogue
		// records pointing at external storage). When per-bucket
		// usage instrumentation lands, this is where the sum goes.
	}
	if a.fipReg != nil {
		for range a.fipReg.listForProject(projectUUID) {
			out.FloatingIPs++
		}
	}
	// RegistryGiB stays 0 : OCI image registries are cluster-wide
	// (no per-project storage cost tracked yet). Future per-project
	// image-cache accounting would aggregate here.
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
	if cap.VolumeGiB <= 0 && cap.VolumeCount <= 0 {
		return nil
	}
	alloc := a.projectAllocation(projectUUID)
	if cap.VolumeGiB > 0 && alloc.VolumeGiB+sizeGiB > cap.VolumeGiB {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: volume_gib (allocated %d + requested %d > cap %d)",
			alloc.VolumeGiB, sizeGiB, cap.VolumeGiB)
	}
	if cap.VolumeCount > 0 && alloc.VolumeCount+1 > cap.VolumeCount {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: volumes (allocated %d + requested 1 > cap %d)",
			alloc.VolumeCount, cap.VolumeCount)
	}
	return nil
}

// EnforceTenantQuotaForShare returns ResourceExhausted when admitting
// a share of `sizeGiB` would push the project past its shares (count)
// or shares_gib caps. Zero caps short-circuit per axis.
func (a *Adapter) EnforceTenantQuotaForShare(projectUUID string, sizeGiB int) error {
	cap := a.TenantQuota(projectUUID)
	if cap.ShareCount <= 0 && cap.ShareGiB <= 0 {
		return nil
	}
	alloc := a.projectAllocation(projectUUID)
	if cap.ShareCount > 0 && alloc.ShareCount+1 > cap.ShareCount {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: shares (allocated %d + requested 1 > cap %d)",
			alloc.ShareCount, cap.ShareCount)
	}
	if cap.ShareGiB > 0 && alloc.ShareGiB+sizeGiB > cap.ShareGiB {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: shares_gib (allocated %d + requested %d > cap %d)",
			alloc.ShareGiB, sizeGiB, cap.ShareGiB)
	}
	return nil
}

// EnforceTenantQuotaForBucket returns ResourceExhausted when admitting
// a new bucket would push the project past its buckets (count) cap.
// buckets_gib has no enforcement path today : the local Bucket struct
// doesn't carry a size dimension (S3 buckets are catalogue records
// pointing at external storage). When per-bucket usage instrumentation
// lands, gate the size delta here.
func (a *Adapter) EnforceTenantQuotaForBucket(projectUUID string) error {
	cap := a.TenantQuota(projectUUID)
	if cap.BucketCount <= 0 {
		return nil
	}
	alloc := a.projectAllocation(projectUUID)
	if alloc.BucketCount+1 > cap.BucketCount {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: buckets (allocated %d + requested 1 > cap %d)",
			alloc.BucketCount, cap.BucketCount)
	}
	return nil
}

// EnforceTenantQuotaForFloatingIP returns ResourceExhausted when
// admitting a new floating IP allocation would push the project
// past its floating_ips cap. Zero cap is "no limit".
func (a *Adapter) EnforceTenantQuotaForFloatingIP(projectUUID string) error {
	cap := a.TenantQuota(projectUUID)
	if cap.FloatingIPs <= 0 {
		return nil
	}
	alloc := a.projectAllocation(projectUUID)
	if alloc.FloatingIPs+1 > cap.FloatingIPs {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: floating_ips (allocated %d + requested 1 > cap %d)",
			alloc.FloatingIPs, cap.FloatingIPs)
	}
	return nil
}

// EnforceTenantQuotaForGPU returns ResourceExhausted when admitting
// a VM whose `requestedGPUs` slice would push the project's
// gpu_count / gpu_memory_gib allocation past its caps. Zero caps
// mean "no limit on this dimension". A nil/empty slice is a
// no-op (re-checks the existing allocation against the cap, the
// RegisterMicroVM no-cpu/mem pattern).
//
// Aggregate enforcement : sums the to-be-added (Count, memory)
// from the slice + the already-allocated total from
// projectAllocation, mirroring how EnforceTenantQuotaForVM and
// EnforceTenantQuotaForVolume frame their delta vs cap
// comparison. This catches both the "single VM asking for 8
// GPUs against a 4 GPU cap" (per-request) and the "fourth 1-GPU
// VM pushing the project from 3 to 4 against a 3 GPU cap"
// (aggregate) cases the per-request-only predecessor missed.
func (a *Adapter) EnforceTenantQuotaForGPU(projectUUID string, requestedGPUs []GPURequest) error {
	cap := a.TenantQuota(projectUUID)
	if cap.GPUCount <= 0 && cap.GPUMemoryGiB <= 0 {
		return nil
	}
	var deltaCount, deltaMem int
	for _, g := range requestedGPUs {
		c := g.Count
		if c <= 0 {
			c = 1
		}
		deltaCount += c
		deltaMem += gpuRequestMemoryGiB(g)
	}
	alloc := a.projectAllocation(projectUUID)
	if cap.GPUCount > 0 && alloc.GPUCount+deltaCount > cap.GPUCount {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: gpu_count (allocated %d + requested %d > cap %d)",
			alloc.GPUCount, deltaCount, cap.GPUCount)
	}
	if cap.GPUMemoryGiB > 0 && alloc.GPUMemoryGiB+deltaMem > cap.GPUMemoryGiB {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: gpu_memory_gib (allocated %d + requested %d > cap %d)",
			alloc.GPUMemoryGiB, deltaMem, cap.GPUMemoryGiB)
	}
	return nil
}

// EnforceTenantQuotaForPCI returns ResourceExhausted when admitting
// a VM whose `requestedPCI` slice would push the project's pci_count
// allocation past its cap. Zero cap means "no limit". A nil/empty
// slice is a no-op (re-checks the existing allocation against the
// cap, the RegisterMicroVM no-cpu/mem pattern).
//
// Aggregate enforcement : sums the to-be-added Count from the slice
// + the already-allocated total from projectAllocation. Mirrors
// EnforceTenantQuotaForGPU's count-axis half — PCI has no per-card
// memory dimension (NIC bandwidth + NVMe capacity aren't sum-able
// usefully under one cap), so the helper is single-dimension. Both
// the per-request (single VM × N PCI > cap) and aggregate (n VMs ×
// 1 PCI > cap) paths trip via the standard delta + alloc framing.
func (a *Adapter) EnforceTenantQuotaForPCI(projectUUID string, requestedPCI []PCIRequest) error {
	cap := a.TenantQuota(projectUUID)
	if cap.PCICount <= 0 {
		return nil
	}
	var deltaCount int
	for _, p := range requestedPCI {
		c := p.Count
		if c <= 0 {
			c = 1
		}
		deltaCount += c
	}
	alloc := a.projectAllocation(projectUUID)
	if alloc.PCICount+deltaCount > cap.PCICount {
		return status.Errorf(codes.ResourceExhausted,
			"tenant quota exhausted: pci_count (allocated %d + requested %d > cap %d)",
			alloc.PCICount, deltaCount, cap.PCICount)
	}
	return nil
}
