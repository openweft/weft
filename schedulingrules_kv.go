package weft

// schedulingrules_kv.go is the per-record (KVStorage) path the
// SchedulingRule registry takes when its Storage backend
// implements KVStorage. Each rule becomes one etcd key at
// /weft/scheduling_rules/<uuid> ; mutations Put/Delete a single
// record instead of rewriting the whole JSON blob.
//
// Schema choice : the legacy blob is JSON (the only registry on
// this side that isn't HCL today) because RespawnPolicy +
// HealthProbe are nested pointer structs that didn't repay the
// effort of hand-rolling HCL blocks. The per-record path moves to
// HCL for the human-readable top-level attributes (selector, target
// count, anti_affinity) and keeps respawn as a one-shot JSON
// attribute — best of both : operators eyeballing a record see the
// scheduling shape on the first line, the rich respawn policy stays
// faithful to the in-memory shape via json.Marshal round-trip.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// schedulingRuleRecordDoc / schedulingRuleRecordBlock are the per-
// record HCL shape. One block per key.
type schedulingRuleRecordDoc struct {
	Rules []schedulingRuleRecordBlock `hcl:"scheduling_rule,block"`
}

type schedulingRuleRecordBlock struct {
	UUID         string `hcl:",label"`
	Name         string `hcl:"name"`
	Selector     string `hcl:"selector,optional"`
	TargetCount  int32  `hcl:"target_count,optional"`
	AntiAffinity string `hcl:"anti_affinity,optional"`
	// RespawnJSON carries a json-marshalled RespawnPolicyJSON when
	// non-empty. Stored as a string attribute because the policy is
	// a nested pointer struct + slice of HealthProbe ; an HCL block
	// encoding would multiply boilerplate without buying anything
	// over the legacy blob path's JSON.
	RespawnJSON string `hcl:"respawn_json,optional"`
	CreatedAt   string `hcl:"created_at"`
}

// encodeSchedulingRuleRecord serialises a single rule as a one-block
// HCL document. The top-level attributes mirror the SchedulingRuleEntry
// JSON tags (selector, target_count, anti_affinity, created_at) so the
// HCL stays operator-readable ; respawn round-trips through JSON.
func encodeSchedulingRuleRecord(sr SchedulingRuleEntry) []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	block := body.AppendNewBlock("scheduling_rule", []string{sr.UUID})
	bb := block.Body()
	bb.SetAttributeValue("name", cty.StringVal(sr.Name))
	if sr.Selector != "" {
		bb.SetAttributeValue("selector", cty.StringVal(sr.Selector))
	}
	if sr.TargetCount != 0 {
		bb.SetAttributeValue("target_count", cty.NumberIntVal(int64(sr.TargetCount)))
	}
	if sr.AntiAffinity != "" {
		bb.SetAttributeValue("anti_affinity", cty.StringVal(sr.AntiAffinity))
	}
	if sr.Respawn != nil {
		// Marshal-error here is treated as "no respawn" rather than
		// corrupting the whole record ; in practice RespawnPolicyJSON
		// is plain types that always marshal cleanly.
		if blob, err := json.Marshal(sr.Respawn); err == nil {
			bb.SetAttributeValue("respawn_json", cty.StringVal(string(blob)))
		}
	}
	bb.SetAttributeValue("created_at", cty.StringVal(sr.CreatedAt.Format(time.RFC3339Nano)))
	return f.Bytes()
}

// decodeSchedulingRuleRecord parses a one-block HCL document into a
// SchedulingRuleEntry. Returns an error if the document carries
// zero or more than one block — the per-record contract is exactly
// one rule per key.
func decodeSchedulingRuleRecord(blob []byte) (SchedulingRuleEntry, error) {
	var doc schedulingRuleRecordDoc
	if err := hclsimple.Decode("scheduling-rule-record.hcl", blob, nil, &doc); err != nil {
		return SchedulingRuleEntry{}, fmt.Errorf("decode scheduling rule record: %w", err)
	}
	if len(doc.Rules) != 1 {
		return SchedulingRuleEntry{}, fmt.Errorf("scheduling rule record: want exactly 1 block, got %d", len(doc.Rules))
	}
	b := doc.Rules[0]
	ts, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
	sr := SchedulingRuleEntry{
		UUID:         b.UUID,
		Name:         b.Name,
		Selector:     b.Selector,
		TargetCount:  b.TargetCount,
		AntiAffinity: b.AntiAffinity,
		CreatedAt:    ts,
	}
	if b.RespawnJSON != "" {
		var rp RespawnPolicyJSON
		if err := json.Unmarshal([]byte(b.RespawnJSON), &rp); err == nil {
			sr.Respawn = &rp
		}
	}
	return sr, nil
}

// loadSchedulingRuleRegistryKV is the per-record load path. Used
// when the kvStorageFactory returns a KVStorage. Falls through to a
// one-time legacy-blob migration if the per-record prefix is empty
// AND a legacy blob exists ; otherwise reads each record via List
// and indexes them.
func loadSchedulingRuleRegistryKV(ctx context.Context, kv KVStorage, legacy Storage) (*schedulingRuleRegistry, error) {
	reg := &schedulingRuleRegistry{
		storage: legacy, // kept for back-compat callers that read r.storage
		kv:      kv,
		byUUID:  make(map[string]SchedulingRuleEntry),
		nameIdx: make(map[string]string),
	}
	records, err := kv.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 && legacy != nil {
		// One-shot migration : legacy blob exists, per-record prefix
		// is empty. Read the blob, write each rule as a per-record
		// key, delete the legacy blob. Idempotent : the next startup
		// finds records under the prefix and skips this branch.
		blob, err := legacy.Load(ctx)
		if err == nil && len(blob) > 0 {
			if err := migrateSchedulingRuleBlobToKV(ctx, blob, kv); err != nil {
				return nil, fmt.Errorf("migrate scheduling rule registry blob → kv: %w", err)
			}
			records, err = kv.List(ctx)
			if err != nil {
				return nil, err
			}
			_ = legacy.Save(ctx, nil)
		}
	}
	for _, blob := range records {
		sr, err := decodeSchedulingRuleRecord(blob)
		if err != nil {
			// Skip a corrupted record rather than failing the whole load.
			continue
		}
		reg.byUUID[sr.UUID] = sr
		reg.nameIdx[sr.Name] = sr.UUID
	}
	return reg, nil
}

// migrateSchedulingRuleBlobToKV walks every rule in the legacy
// JSON blob and writes it under the per-record prefix in the HCL
// shape decodeSchedulingRuleRecord expects.
func migrateSchedulingRuleBlobToKV(ctx context.Context, blob []byte, kv KVStorage) error {
	var doc schedulingRulesDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return fmt.Errorf("parse legacy blob: %w", err)
	}
	for _, sr := range doc.Rules {
		if err := kv.PutOne(ctx, sr.UUID, encodeSchedulingRuleRecord(sr)); err != nil {
			return fmt.Errorf("put migrated scheduling rule %s: %w", sr.UUID, err)
		}
	}
	return nil
}

// persistOne is the per-record save path. Writes a single rule
// record under the KV prefix when r.kv is set ; otherwise falls
// back to the legacy full-blob save. Caller must already hold r.mu
// and have updated the in-memory indices.
func (r *schedulingRuleRegistry) persistOne(sr SchedulingRuleEntry) error {
	if r.kv != nil {
		return r.kv.PutOne(context.Background(), sr.UUID, encodeSchedulingRuleRecord(sr))
	}
	return r.saveLocked()
}

// persistDelete removes a single rule record from the KV backend.
// Falls back to full-blob save in legacy mode.
func (r *schedulingRuleRegistry) persistDelete(uuid string) error {
	if r.kv != nil {
		return r.kv.DeleteOne(context.Background(), uuid)
	}
	return r.saveLocked()
}

// applyKVEvent updates the in-memory indices in response to a per-
// record event the agent received from another DC (or another
// agent on the same cluster writing through its own client).
// Surgical update : only the affected rule's entries change.
//
// On Put : drop the previous in-memory entry (by UUID) so an update
// can't leave a stale name in nameIdx, then re-index the new one.
// On Delete : drop the indices, falling back to PrevValue when the
// post-state is gone (cleans up nameIdx by the previous Name).
func (r *schedulingRuleRegistry) applyKVEvent(ev KVEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case KVPut:
		sr, err := decodeSchedulingRuleRecord(ev.Value)
		if err != nil {
			return
		}
		if prev, ok := r.byUUID[sr.UUID]; ok {
			delete(r.nameIdx, prev.Name)
		}
		r.byUUID[sr.UUID] = sr
		r.nameIdx[sr.Name] = sr.UUID
	case KVDelete:
		uuid := keyToUUID(ev.Key, r.kv.Prefix())
		if prev, ok := r.byUUID[uuid]; ok {
			delete(r.byUUID, uuid)
			delete(r.nameIdx, prev.Name)
			return
		}
		if ev.PrevValue != nil {
			if prev, err := decodeSchedulingRuleRecord(ev.PrevValue); err == nil {
				delete(r.byUUID, prev.UUID)
				delete(r.nameIdx, prev.Name)
			}
		}
	}
}
