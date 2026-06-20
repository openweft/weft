package weft

// gpu_alloc_kv.go is the per-record (KVStorage) persistence path for the
// GPU claim table. Each live claim becomes one etcd key at
// /weft/gpu_allocations/<host>~<resource> ; the value is a one-block HCL
// document mirroring GPUClaim. Mirrors schedulingrules_kv.go's shape so
// operators eyeballing a record see the holder on the first lines.
//
// The record key uses "~" (not the in-memory claimKey's "/") so a PCI BDF
// or MIG UUID can't introduce a path separator into the etcd key. The
// host + resource are also stored as attributes, so decode reconstructs
// the in-memory claimKey without parsing the record key back apart.

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// claimRecordKey is the per-record KV key for a claim. Distinct from the
// in-memory claimKey (which uses "/") so the resource id — a BDF
// ("0000:65:00.0") or MIG UUID — never injects a path separator.
func claimRecordKey(c GPUClaim) string {
	return c.HostUUID + "~" + c.ResourceID
}

// gpuClaimRecordDoc / Block are the per-record HCL shape : one block per
// key.
type gpuClaimRecordDoc struct {
	Claims []gpuClaimRecordBlock `hcl:"gpu_claim,block"`
}

type gpuClaimRecordBlock struct {
	Key             string `hcl:",label"`
	HostUUID        string `hcl:"host_uuid"`
	ResourceID      string `hcl:"resource_id"`
	Kind            string `hcl:"kind"`
	VMUUID          string `hcl:"vm_uuid"`
	Model           string `hcl:"model,optional"`
	CreatedAtUnixNs string `hcl:"created_at_unix_ns,optional"` // int64-as-string : keeps full precision through cty
}

// encodeGPUClaimRecord serialises one claim as a one-block HCL document.
func encodeGPUClaimRecord(c GPUClaim) []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	bb := body.AppendNewBlock("gpu_claim", []string{claimRecordKey(c)}).Body()
	bb.SetAttributeValue("host_uuid", cty.StringVal(c.HostUUID))
	bb.SetAttributeValue("resource_id", cty.StringVal(c.ResourceID))
	bb.SetAttributeValue("kind", cty.StringVal(string(c.Kind)))
	bb.SetAttributeValue("vm_uuid", cty.StringVal(c.VMUUID))
	if c.Model != "" {
		bb.SetAttributeValue("model", cty.StringVal(c.Model))
	}
	if c.CreatedAtUnixNs != 0 {
		// Stored as a string : an int64 ns timestamp exceeds cty's
		// safe-integer range and would round-trip lossily as a number.
		bb.SetAttributeValue("created_at_unix_ns", cty.StringVal(strconv.FormatInt(c.CreatedAtUnixNs, 10)))
	}
	return f.Bytes()
}

// decodeGPUClaimRecord parses a one-block HCL document into a GPUClaim.
// Errors when the document doesn't carry exactly one block.
func decodeGPUClaimRecord(blob []byte) (GPUClaim, error) {
	var doc gpuClaimRecordDoc
	if err := hclsimple.Decode("gpu-claim-record.hcl", blob, nil, &doc); err != nil {
		return GPUClaim{}, fmt.Errorf("decode gpu claim record: %w", err)
	}
	if len(doc.Claims) != 1 {
		return GPUClaim{}, fmt.Errorf("gpu claim record: want exactly 1 block, got %d", len(doc.Claims))
	}
	b := doc.Claims[0]
	var ts int64
	if b.CreatedAtUnixNs != "" {
		ts, _ = strconv.ParseInt(b.CreatedAtUnixNs, 10, 64)
	}
	return GPUClaim{
		HostUUID:        b.HostUUID,
		ResourceID:      b.ResourceID,
		Kind:            GPUClaimKind(b.Kind),
		VMUUID:          b.VMUUID,
		Model:           b.Model,
		CreatedAtUnixNs: ts,
	}, nil
}

// loadGPUAllocTableKV builds a claim table from the per-record KV prefix.
// Corrupted records are skipped (not fatal) so one bad key can't take the
// whole table down — same resilience as loadSchedulingRuleRegistryKV.
func loadGPUAllocTableKV(ctx context.Context, kv KVStorage) (*gpuAllocTable, error) {
	t := newGPUAllocTableKV(kv)
	records, err := kv.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, blob := range records {
		c, err := decodeGPUClaimRecord(blob)
		if err != nil {
			continue
		}
		if validateClaim(c) != nil {
			continue
		}
		// Hydrate the in-memory indices directly — these records are
		// the source of truth, so a conflict between two persisted
		// records (shouldn't happen) keeps the first and drops the
		// rest rather than erroring the load.
		if t.conflictLocked(c) == nil {
			t.claimLocked(c)
		}
	}
	return t, nil
}
