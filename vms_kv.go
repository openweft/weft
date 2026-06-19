package weft

// vms_kv.go is the per-record (KVStorage) path the VM registry takes
// when its Storage backend implements KVStorage. Each VM becomes
// one etcd key at /weft/vms/<uuid> ; mutations Put/Delete a single
// record instead of rewriting the whole blob. Watchers receive
// per-record diffs and apply them surgically to the in-memory
// indices.
//
// Why it's a separate file : the blob-mode `saveLocked` +
// `reloadFromStorage` stay untouched for back-compat (file backend,
// in-memory tests, legacy etcd deploys). This file adds the per-
// record branches the vmRegistry dispatches to when r.kv != nil.
//
// Migration : a registry that finds both a legacy blob AND a
// populated per-record prefix during load prefers the per-record
// data ; if only the legacy blob is present, the loader writes each
// record under the per-record prefix and deletes the blob. Run-once
// and idempotent — a second startup sees the per-record data only
// and skips the migration entirely.

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// encodeVMRecord serialises a single VM as a one-block HCL document.
// Reuses the exact same attribute layout as the legacy
// vmRegistry.saveLocked so any tool that read the blob can still
// read a per-record key (just one `vm` block per Get).
func encodeVMRecord(v VM) []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	block := body.AppendNewBlock("vm", []string{v.UUID})
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
	return f.Bytes()
}

// decodeVMRecord parses a one-block HCL document into a VM. Returns
// an error if the document carries zero or more than one block —
// the per-record contract is exactly one VM per key.
func decodeVMRecord(blob []byte) (VM, error) {
	var doc vmsDoc
	if err := hclsimple.Decode("vm-record.hcl", blob, nil, &doc); err != nil {
		return VM{}, fmt.Errorf("decode vm record: %w", err)
	}
	if len(doc.VMs) != 1 {
		return VM{}, fmt.Errorf("vm record: want exactly 1 block, got %d", len(doc.VMs))
	}
	b := doc.VMs[0]
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
	return VM{
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
	}, nil
}

// loadVMRegistryKV is the per-record load path. Used when the
// storageFactory returns a KVStorage instead of a blob Storage. Falls
// through to a one-time legacy-blob migration if the per-record
// prefix is empty AND a legacy blob exists ; otherwise reads each
// record via List and indexes them.
func loadVMRegistryKV(ctx context.Context, kv KVStorage, legacy Storage) (*vmRegistry, error) {
	reg := &vmRegistry{
		storage:    legacy, // kept for back-compat callers that read r.storage
		kv:         kv,
		byUUID:     make(map[string]VM),
		nameIdx:    make(map[string]string),
		projectIdx: make(map[string]map[string]struct{}),
		hostIdx:    make(map[string]map[string]struct{}),
	}
	records, err := kv.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 && legacy != nil {
		// One-shot migration : legacy blob exists, per-record prefix
		// is empty. Read the blob, write each VM as a per-record
		// key, delete the legacy blob. Idempotent : the next startup
		// finds records under the prefix and skips this branch.
		blob, err := legacy.Load(ctx)
		if err == nil && len(blob) > 0 {
			if err := migrateBlobToKV(ctx, blob, kv); err != nil {
				return nil, fmt.Errorf("migrate vm registry blob → kv: %w", err)
			}
			// Re-list after migration so the in-memory state matches
			// what the persistent layer just gained.
			records, err = kv.List(ctx)
			if err != nil {
				return nil, err
			}
			// Best-effort delete of the legacy blob. A leftover blob
			// would just be ignored next boot but it's cleaner to
			// remove it now.
			_ = legacy.Save(ctx, nil)
		}
	}
	for _, blob := range records {
		v, err := decodeVMRecord(blob)
		if err != nil {
			// Skip a corrupted record rather than failing the whole
			// load — other agents on the same etcd can rewrite it.
			continue
		}
		reg.indexLocked(v)
	}
	return reg, nil
}

// migrateBlobToKV walks every VM block in the legacy HCL blob and
// writes it under the per-record prefix. Pure side-effect ; no
// in-memory state touched (the caller re-lists after).
func migrateBlobToKV(ctx context.Context, blob []byte, kv KVStorage) error {
	var doc vmsDoc
	if err := hclsimple.Decode("vm-registry.hcl", blob, nil, &doc); err != nil {
		return fmt.Errorf("parse legacy blob: %w", err)
	}
	for _, b := range doc.VMs {
		// Roundtrip through decodeVMRecord-equivalent by serialising
		// each block individually : reuse encodeVMRecord against a
		// freshly-constructed VM with the same fields.
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
		if err := kv.PutOne(ctx, v.UUID, encodeVMRecord(v)); err != nil {
			return fmt.Errorf("put migrated vm %s: %w", v.UUID, err)
		}
	}
	return nil
}

// persistOne is the per-record save path. Writes a single VM record
// under the KV prefix ; called by every mutation method (create,
// setHost, setName, setState) in KV mode. Falls back to the legacy
// full-blob save when r.kv is nil — back-compat with FileStorage +
// MemStorage tests.
//
// Caller must already hold r.mu and have updated the in-memory
// indices ; persistOne only handles the storage side.
func (r *vmRegistry) persistOne(v VM) error {
	if r.kv != nil {
		return r.kv.PutOne(context.Background(), v.UUID, encodeVMRecord(v))
	}
	return r.saveLocked()
}

// persistDelete removes a single VM record from the KV backend.
// Falls back to full-blob save in legacy mode.
func (r *vmRegistry) persistDelete(uuid string) error {
	if r.kv != nil {
		return r.kv.DeleteOne(context.Background(), uuid)
	}
	return r.saveLocked()
}

// applyKVEvent updates the in-memory indices in response to a per-
// record event the agent received from another DC (or another agent
// on the same cluster writing through its own client). Surgical
// update : only the affected VM's entries change ; we don't rebuild
// the four maps from scratch like reloadFromStorage does.
//
// On Put : index the new VM, removing the previous entry first if a
// VM with the same UUID was already indexed (handles ownership
// claims that flip host_uuid without changing the key).
//
// On Delete : drop the indices, falling back to the previous-blob
// fields when available (the etcd Delete event carries PrevKv) so we
// can clean up the host/name indices even when the post-state is
// gone.
func (r *vmRegistry) applyKVEvent(ev KVEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case KVPut:
		v, err := decodeVMRecord(ev.Value)
		if err != nil {
			return
		}
		// Remove the previous in-memory entry first — handles the
		// host_uuid flip during a claim.
		if prev, ok := r.byUUID[v.UUID]; ok {
			r.unindexLocked(prev)
		}
		r.indexLocked(v)
	case KVDelete:
		uuid := keyToUUID(ev.Key, r.kv.Prefix())
		if prev, ok := r.byUUID[uuid]; ok {
			r.unindexLocked(prev)
			return
		}
		if ev.PrevValue != nil {
			if prev, err := decodeVMRecord(ev.PrevValue); err == nil {
				r.unindexLocked(prev)
			}
		}
	}
}

// keyToUUID extracts the record id from a full etcd key by stripping
// the registry's prefix. Defensive : if the key doesn't carry the
// prefix (shouldn't happen for a watcher's events) return the whole
// key unchanged so the caller can still attempt cleanup.
func keyToUUID(key, prefix string) string {
	if len(key) > len(prefix) && bytes.HasPrefix([]byte(key), []byte(prefix)) {
		return key[len(prefix):]
	}
	return key
}

// listKeys returns the sorted record ids the KV backend currently
// holds. Diagnostic helper ; not on the hot path.
func (r *vmRegistry) listKeys(ctx context.Context) ([]string, error) {
	if r.kv == nil {
		return nil, fmt.Errorf("vm registry not on a KV backend")
	}
	records, err := r.kv.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(records))
	for k := range records {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
