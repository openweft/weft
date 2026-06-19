package main

import (
	"testing"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
)

// TestQuotaConverters_ProtoLocalRoundTrip pins the field mapping
// between the proto Quotas (10 dims operator-set + 1 tenant-only
// `projects` which isn't on TenantQuota) and the local TenantQuota
// (10 proto-aligned + 3 local-only GPU/PCI). The 10 overlapping
// dimensions must round-trip without loss.
func TestQuotaConverters_ProtoLocalRoundTrip(t *testing.T) {
	in := &weftv1.Quotas{
		Vcpu: 64, RamGib: 256, Volumes: 30, VolumesGib: 2048,
		Shares: 8, SharesGib: 512, Buckets: 4, BucketsGib: 1024,
		RegistryGib: 50, FloatingIps: 12,
	}
	mid := protoToTenantQuota(in)
	out := tenantQuotaToProto(mid)
	if in.Vcpu != out.Vcpu || in.RamGib != out.RamGib ||
		in.Volumes != out.Volumes || in.VolumesGib != out.VolumesGib ||
		in.Shares != out.Shares || in.SharesGib != out.SharesGib ||
		in.Buckets != out.Buckets || in.BucketsGib != out.BucketsGib ||
		in.RegistryGib != out.RegistryGib || in.FloatingIps != out.FloatingIps {
		t.Errorf("proto Quotas not round-trip preserved\n  in  : %+v\n  out : %+v", in, out)
	}
}

// TestQuotaConverters_LocalOnlyDimensionsStrippedOnWire : GPUs and
// PCI count have no proto representation, so tenantQuotaToProto
// drops them. Pin that to avoid a regression where someone adds
// GPU fields to weftv1.Quotas + the converter quietly maps to the
// wrong field number.
func TestQuotaConverters_LocalOnlyDimensionsStrippedOnWire(t *testing.T) {
	in := weft.TenantQuota{
		CPUCount: 4, GPUCount: 2, GPUMemoryGiB: 80, PCICount: 1,
	}
	out := tenantQuotaToProto(in)
	if out.Vcpu != 4 {
		t.Errorf("Vcpu = %d, want 4", out.Vcpu)
	}
	// The proto has no GPU/PCI columns ; if it grows them this
	// test should adapt + the converter should map them.
}

// TestQuotaConverters_NilProtoSafeToConvert : a Set RPC carrying
// a nil Quotas means "clear the cap" ; protoToTenantQuota must
// return the zero value without panicking.
func TestQuotaConverters_NilProtoSafeToConvert(t *testing.T) {
	got := protoToTenantQuota(nil)
	if got != (weft.TenantQuota{}) {
		t.Errorf("nil proto = %+v, want zero", got)
	}
}
