package main

// quotas.go owns the server-side quota RPCs (Get/Set Project /
// Tenant Quota). The local TenantQuota carries 11 dimensions
// (the 10 from weftv1.Quotas plus 3 local-only ones :
// GPUCount, GPUMemoryGiB, PCICount). The proto round-trip is
// lossless for the 10 proto-aligned dimensions ; the 3 local
// extras stay in HCL persistence + the EnforceTenantQuotaForVM
// helpers, invisible on the wire.
//
// Project quotas use the existing project-keyed registry the
// VM/Volume enforcement path already reads (adapter.TenantQuota
// / SetTenantQuota, "tenant" here is the historical name for
// what's morally a project-level cap — see top of tenant_quotas.go).
//
// Tenant quotas (true tenant-scoped caps, with siblings_total
// aggregation across a tenant's projects) need a project →
// tenant linkage that doesn't exist in the codebase yet (see
// [[project-proto-v0120-properties-restartvm]] memory) — those
// RPCs return Unimplemented with a clearer error than the
// generated stub so operators know it's a missing-feature gap,
// not a bug.

import (
	"context"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// protoToTenantQuota converts a proto Quotas into the local
// TenantQuota. The 3 local-only dimensions (GPUCount,
// GPUMemoryGiB, PCICount) stay at their prior values — callers
// that want to clear them go through the dedicated CLI surface.
// Returns the zero value when proto is nil so a Set with a missing
// Quotas message clears the cap (the CLI's `weft quota project
// set` always passes a non-nil Quotas after merging GET first).
func protoToTenantQuota(p *weftv1.Quotas) weft.TenantQuota {
	if p == nil {
		return weft.TenantQuota{}
	}
	return weft.TenantQuota{
		CPUCount:    int(p.Vcpu),
		MemoryGiB:   int(p.RamGib),
		VolumeCount: int(p.Volumes),
		VolumeGiB:   int(p.VolumesGib),
		ShareCount:  int(p.Shares),
		ShareGiB:    int(p.SharesGib),
		BucketCount: int(p.Buckets),
		BucketGiB:   int(p.BucketsGib),
		RegistryGiB: int(p.RegistryGib),
		FloatingIPs: int(p.FloatingIps),
	}
}

// tenantQuotaToProto is the inverse. Local-only dimensions are
// dropped — the wire shape has no field for them.
func tenantQuotaToProto(q weft.TenantQuota) *weftv1.Quotas {
	return &weftv1.Quotas{
		Vcpu:        int32(q.CPUCount),
		RamGib:      int32(q.MemoryGiB),
		Volumes:     int32(q.VolumeCount),
		VolumesGib:  int32(q.VolumeGiB),
		Shares:      int32(q.ShareCount),
		SharesGib:   int32(q.ShareGiB),
		Buckets:     int32(q.BucketCount),
		BucketsGib:  int32(q.BucketGiB),
		RegistryGib: int32(q.RegistryGiB),
		FloatingIps: int32(q.FloatingIPs),
	}
}

// GetProjectQuota returns the project's quota cap + the currently-
// allocated values. The proto envelope also carries `tenant_cap`
// and `siblings_total` (the parent tenant's cap, and the sum of
// other projects in the tenant), but those require a project →
// tenant linkage that doesn't exist yet — both fields stay nil
// until the linkage lands and the CLI / webui handle the partial
// response gracefully (the `weft quota project get` renderer treats
// nil tenant_cap as "no tenant context").
func (s *weftServer) GetProjectQuota(ctx context.Context, req *weftv1.GetProjectQuotaRequest) (*weftv1.GetProjectQuotaResponse, error) {
	if err := weft.RequireAdmin(ctx, "get project quota"); err != nil {
		return nil, err
	}
	if req.ProjectUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "project_uuid is required")
	}
	cap := s.adp.TenantQuota(req.ProjectUuid)
	resp := &weftv1.GetProjectQuotaResponse{
		Project: tenantQuotaToProto(cap),
	}
	// Aggregate siblings_total when the project is bound to a
	// tenant. siblings = every OTHER project in the same tenant ;
	// we sum their quotas (proto-aligned dimensions only) into the
	// response. Untenanted projects (TenantUUID=="") keep
	// SiblingsTotal nil so clients can distinguish "no aggregation
	// possible" from "tenant exists but has no siblings".
	proj, ok := s.adp.ProjectByUUID(req.ProjectUuid)
	if ok && proj.TenantUUID != "" {
		sum := weft.TenantQuota{}
		for _, sib := range s.adp.ProjectsByTenant(proj.TenantUUID) {
			if sib.UUID == req.ProjectUuid {
				continue
			}
			q := s.adp.TenantQuota(sib.UUID)
			sum.CPUCount += q.CPUCount
			sum.MemoryGiB += q.MemoryGiB
			sum.VolumeCount += q.VolumeCount
			sum.VolumeGiB += q.VolumeGiB
			sum.ShareCount += q.ShareCount
			sum.ShareGiB += q.ShareGiB
			sum.BucketCount += q.BucketCount
			sum.BucketGiB += q.BucketGiB
			sum.RegistryGiB += q.RegistryGiB
			sum.FloatingIPs += q.FloatingIPs
		}
		resp.SiblingsTotal = tenantQuotaToProto(sum)
		// TenantCap stays nil until a tenant-level cap registry lands ;
		// that's the next milestone after this linkage ships.
	}
	return resp, nil
}

// SetProjectQuota replaces the project's quota cap atomically.
// Returns the just-set quota (echoes the input post-validation) so
// the CLI can show the operator what landed. Unknown project UUIDs
// are intentionally permitted — operators commonly stage caps
// before project creation.
func (s *weftServer) SetProjectQuota(ctx context.Context, req *weftv1.SetProjectQuotaRequest) (*weftv1.SetProjectQuotaResponse, error) {
	if err := weft.RequireAdmin(ctx, "set project quota"); err != nil {
		return nil, err
	}
	if req.ProjectUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "project_uuid is required")
	}
	q := protoToTenantQuota(req.Quota)
	// Preserve local-only dimensions (GPU/PCI) from the prior cap —
	// the proto Quotas doesn't carry them, so a Set via the wire
	// would zero them out unless we merge.
	prior := s.adp.TenantQuota(req.ProjectUuid)
	q.GPUCount = prior.GPUCount
	q.GPUMemoryGiB = prior.GPUMemoryGiB
	q.PCICount = prior.PCICount
	if err := s.adp.SetTenantQuota(req.ProjectUuid, q); err != nil {
		return nil, status.Errorf(codes.Internal, "set project quota: %v", err)
	}
	return &weftv1.SetProjectQuotaResponse{
		Project: tenantQuotaToProto(q),
	}, nil
}

// GetTenantQuota / SetTenantQuota return Unimplemented until the
// project → tenant linkage lands. Surfacing it as Unimplemented (a
// gRPC error code clients should already special-case) with an
// explicit message beats letting the legacy generated stub fire
// the bare "method GetTenantQuota not implemented" line — operators
// hitting `weft quota tenant get` now see WHY it's not wired.
func (s *weftServer) GetTenantQuota(ctx context.Context, _ *weftv1.GetTenantQuotaRequest) (*weftv1.GetTenantQuotaResponse, error) {
	if err := weft.RequireAdmin(ctx, "get tenant quota"); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.Unimplemented,
		"tenant-scoped quotas need a project→tenant linkage that doesn't exist yet ; "+
			"use `weft quota project get/set` until the Project struct grows a TenantUUID field")
}

func (s *weftServer) SetTenantQuota(ctx context.Context, _ *weftv1.SetTenantQuotaRequest) (*weftv1.SetTenantQuotaResponse, error) {
	if err := weft.RequireAdmin(ctx, "set tenant quota"); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.Unimplemented,
		"tenant-scoped quotas need a project→tenant linkage that doesn't exist yet ; "+
			"use `weft quota project get/set` until the Project struct grows a TenantUUID field")
}
