package weft

// tenant_caps.go is the tenant-keyed cap registry that complements
// tenant_quotas.go (despite the name, the latter is project-keyed —
// see tenant_quotas.go top-of-file for the historical rationale).
//
// One tenant_cap "<tenant-uuid>" block per tenant ; the cap shape is
// the same TenantQuota struct we already persist at the project
// level. Storage uses the "tenant-caps" registry key, distinct from
// "tenant-quotas", so the two registries can be backed by different
// blobs (file paths or etcd keys) without colliding.
//
// Wired into the agent surface via :
//   * Adapter.TenantCap(tenantUUID)         — read
//   * Adapter.SetTenantCap(tenantUUID, q)   — write
//   * Adapter.TenantAllocation(tenantUUID)  — sum across projects in the tenant
//
// GetTenantQuotaResponse.Cap maps to TenantCap, .Allocated to
// TenantAllocation. SetTenantQuota writes via SetTenantCap.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

type tenantCapDoc struct {
	Caps []tenantCapBlock `hcl:"tenant_cap,block"`
}

type tenantCapBlock struct {
	TenantUUID   string `hcl:",label"`
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

type tenantCapRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]TenantQuota
}

func loadTenantCapRegistry(ctx context.Context, storage Storage) (*tenantCapRegistry, error) {
	reg := &tenantCapRegistry{
		storage: storage,
		byUUID:  make(map[string]TenantQuota),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load tenant-cap registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc tenantCapDoc
	if err := hclsimple.Decode("tenant-caps.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse tenant-cap registry: %w", err)
	}
	for _, b := range doc.Caps {
		reg.byUUID[b.TenantUUID] = TenantQuota{
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

func (r *tenantCapRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		q := r.byUUID[u]
		block := body.AppendNewBlock("tenant_cap", []string{u})
		bb := block.Body()
		setIfPos := func(name string, v int) {
			if v > 0 {
				bb.SetAttributeValue(name, cty.NumberIntVal(int64(v)))
			}
		}
		setIfPos("cpu_count", q.CPUCount)
		setIfPos("memory_gib", q.MemoryGiB)
		setIfPos("volume_gib", q.VolumeGiB)
		setIfPos("gpu_count", q.GPUCount)
		setIfPos("gpu_memory_gib", q.GPUMemoryGiB)
		setIfPos("pci_count", q.PCICount)
		setIfPos("volume_count", q.VolumeCount)
		setIfPos("share_count", q.ShareCount)
		setIfPos("share_gib", q.ShareGiB)
		setIfPos("bucket_count", q.BucketCount)
		setIfPos("bucket_gib", q.BucketGiB)
		setIfPos("registry_gib", q.RegistryGiB)
		setIfPos("floating_ips", q.FloatingIPs)
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

func (r *tenantCapRegistry) get(tenantUUID string) TenantQuota {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byUUID[tenantUUID]
}

func (r *tenantCapRegistry) set(tenantUUID string, q TenantQuota) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if isZeroQuota(q) {
		delete(r.byUUID, tenantUUID)
	} else {
		r.byUUID[tenantUUID] = q
	}
	return r.saveLocked()
}

func isZeroQuota(q TenantQuota) bool {
	return q.CPUCount == 0 && q.MemoryGiB == 0 && q.VolumeGiB == 0 &&
		q.GPUCount == 0 && q.GPUMemoryGiB == 0 && q.PCICount == 0 &&
		q.VolumeCount == 0 && q.ShareCount == 0 && q.ShareGiB == 0 &&
		q.BucketCount == 0 && q.BucketGiB == 0 && q.RegistryGiB == 0 &&
		q.FloatingIPs == 0
}

// TenantCap returns the configured cap for a tenant. Zero value (no
// caps on any dimension) for an unknown tenant — operators read the
// zero shape as "no limit", same convention as TenantQuota.
func (a *Adapter) TenantCap(tenantUUID string) TenantQuota {
	if a.tenantCaps == nil {
		return TenantQuota{}
	}
	return a.tenantCaps.get(tenantUUID)
}

// SetTenantCap atomically replaces the tenant-level cap. Passing
// the zero TenantQuota clears every dimension (the registry treats
// it as a delete).
func (a *Adapter) SetTenantCap(tenantUUID string, q TenantQuota) error {
	if a.tenantCaps == nil {
		return fmt.Errorf("tenant-cap registry not initialised")
	}
	return a.tenantCaps.set(tenantUUID, q)
}

// TenantAllocation sums the per-project quota caps across every
// project bound to the tenant. Used by GetTenantQuota.Allocated.
// The result mirrors what an operator would see by manually
// summing `weft quota project get` across the tenant's projects.
// Returns the zero value for an unknown tenant or one with no
// bound projects.
func (a *Adapter) TenantAllocation(tenantUUID string) TenantQuota {
	if a.tenantQuotas == nil {
		return TenantQuota{}
	}
	out := TenantQuota{}
	for _, p := range a.ProjectsByTenant(tenantUUID) {
		q := a.tenantQuotas.get(p.UUID)
		out.CPUCount += q.CPUCount
		out.MemoryGiB += q.MemoryGiB
		out.VolumeGiB += q.VolumeGiB
		out.GPUCount += q.GPUCount
		out.GPUMemoryGiB += q.GPUMemoryGiB
		out.PCICount += q.PCICount
		out.VolumeCount += q.VolumeCount
		out.ShareCount += q.ShareCount
		out.ShareGiB += q.ShareGiB
		out.BucketCount += q.BucketCount
		out.BucketGiB += q.BucketGiB
		out.RegistryGiB += q.RegistryGiB
		out.FloatingIPs += q.FloatingIPs
	}
	return out
}

func (a *Adapter) initTenantCaps() {
	storage := a.storageFactory("tenant-caps")
	reg, err := loadTenantCapRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load tenant-cap registry: %v\n", err)
		reg = &tenantCapRegistry{
			storage: storage,
			byUUID:  make(map[string]TenantQuota),
		}
	}
	a.tenantCaps = reg
}
