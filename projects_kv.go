package weft

// projects_kv.go is the per-record (KVStorage) path the project
// registry takes when its Storage backend implements KVStorage.
// Each project becomes one etcd key at /weft/projects/<uuid> ;
// mutations Put/Delete a single record instead of rewriting the
// whole blob. Watchers receive per-record diffs and apply them
// surgically to the in-memory indices.
//
// Pattern : mirror of vms_kv.go. See the docstring there for the
// V0.1.4 design rationale. Migration is one-shot and idempotent : a
// registry that finds both a legacy blob AND a populated per-record
// prefix prefers the per-record data ; if only the legacy blob is
// present, the loader writes each record under the per-record
// prefix and deletes the blob.

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// encodeProjectRecord serialises a single project as a one-block
// HCL document. Reuses the exact same attribute layout as the
// legacy projectRegistry.saveLocked so any tool that read the blob
// can still read a per-record key (just one `project` block per
// Get).
func encodeProjectRecord(p Project) []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	block := body.AppendNewBlock("project", []string{p.UUID})
	bb := block.Body()
	bb.SetAttributeValue("name", cty.StringVal(p.Name))
	bb.SetAttributeValue("created_at", cty.StringVal(p.CreatedAt.Format(time.RFC3339Nano)))
	if p.NATSUserSeed != "" {
		bb.SetAttributeValue("nats_user_seed", cty.StringVal(p.NATSUserSeed))
	}
	if len(p.Members) > 0 {
		// Stable-sort for diff-friendly output, same as the blob path.
		members := append([]string(nil), p.Members...)
		for i := 1; i < len(members); i++ {
			for j := i; j > 0 && members[j-1] > members[j]; j-- {
				members[j-1], members[j] = members[j], members[j-1]
			}
		}
		vals := make([]cty.Value, len(members))
		for i, m := range members {
			vals[i] = cty.StringVal(m)
		}
		bb.SetAttributeValue("members", cty.ListVal(vals))
	}
	return f.Bytes()
}

// decodeProjectRecord parses a one-block HCL document into a
// Project. Returns an error if the document carries zero or more
// than one block — the per-record contract is exactly one project
// per key.
func decodeProjectRecord(blob []byte) (Project, error) {
	var doc projectsDoc
	if err := hclsimple.Decode("project-record.hcl", blob, nil, &doc); err != nil {
		return Project{}, fmt.Errorf("decode project record: %w", err)
	}
	if len(doc.Projects) != 1 {
		return Project{}, fmt.Errorf("project record: want exactly 1 block, got %d", len(doc.Projects))
	}
	b := doc.Projects[0]
	ts, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
	return Project{
		UUID:         b.UUID,
		Name:         b.Name,
		CreatedAt:    ts,
		Members:      append([]string(nil), b.Members...),
		NATSUserSeed: b.NATSUserSeed,
	}, nil
}

// loadProjectRegistryKV is the per-record load path. Used when the
// kvStorageFactory returns a KVStorage. Falls through to a one-time
// legacy-blob migration if the per-record prefix is empty AND a
// legacy blob exists ; otherwise reads each record via List and
// indexes them.
func loadProjectRegistryKV(ctx context.Context, kv KVStorage, legacy Storage) (*projectRegistry, error) {
	reg := &projectRegistry{
		storage: legacy, // kept for back-compat callers that read r.storage
		kv:      kv,
		byUUID:  make(map[string]Project),
		nameIdx: make(map[string]string),
	}
	records, err := kv.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 && legacy != nil {
		// One-shot migration : legacy blob exists, per-record prefix
		// is empty. Read the blob, write each project as a per-record
		// key, delete the legacy blob. Idempotent : the next startup
		// finds records under the prefix and skips this branch.
		blob, err := legacy.Load(ctx)
		if err == nil && len(blob) > 0 {
			if err := migrateProjectBlobToKV(ctx, blob, kv); err != nil {
				return nil, fmt.Errorf("migrate project registry blob → kv: %w", err)
			}
			records, err = kv.List(ctx)
			if err != nil {
				return nil, err
			}
			// Best-effort delete of the legacy blob.
			_ = legacy.Save(ctx, nil)
		}
	}
	for _, blob := range records {
		p, err := decodeProjectRecord(blob)
		if err != nil {
			// Skip a corrupted record rather than failing the whole
			// load — other agents on the same etcd can rewrite it.
			continue
		}
		reg.byUUID[p.UUID] = p
		reg.nameIdx[p.Name] = p.UUID
	}
	return reg, nil
}

// migrateProjectBlobToKV walks every project block in the legacy
// HCL blob and writes it under the per-record prefix. Pure side-
// effect ; no in-memory state touched (the caller re-lists after).
func migrateProjectBlobToKV(ctx context.Context, blob []byte, kv KVStorage) error {
	var doc projectsDoc
	if err := hclsimple.Decode("project-registry.hcl", blob, nil, &doc); err != nil {
		return fmt.Errorf("parse legacy blob: %w", err)
	}
	for _, b := range doc.Projects {
		ts, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		p := Project{
			UUID:         b.UUID,
			Name:         b.Name,
			CreatedAt:    ts,
			Members:      append([]string(nil), b.Members...),
			NATSUserSeed: b.NATSUserSeed,
		}
		if err := kv.PutOne(ctx, p.UUID, encodeProjectRecord(p)); err != nil {
			return fmt.Errorf("put migrated project %s: %w", p.UUID, err)
		}
	}
	return nil
}

// persistOne is the per-record save path. Writes a single project
// record under the KV prefix when r.kv is set ; otherwise falls
// back to the legacy full-blob save. Caller must already hold r.mu
// and have updated the in-memory indices.
func (r *projectRegistry) persistOne(p Project) error {
	if r.kv != nil {
		return r.kv.PutOne(context.Background(), p.UUID, encodeProjectRecord(p))
	}
	return r.saveLocked()
}

// persistDelete removes a single project record from the KV
// backend. Falls back to full-blob save in legacy mode.
func (r *projectRegistry) persistDelete(uuid string) error {
	if r.kv != nil {
		return r.kv.DeleteOne(context.Background(), uuid)
	}
	return r.saveLocked()
}

// applyKVEvent updates the in-memory indices in response to a per-
// record event the agent received from another DC (or another
// agent on the same cluster writing through its own client).
// Surgical update : only the affected project's entries change.
//
// On Put : drop the previous in-memory entry (by UUID) so a rename
// can't leave a stale name in nameIdx, then re-index the new one.
// On Delete : drop the indices, falling back to PrevValue when the
// post-state is gone (cleans up nameIdx by the previous Name).
func (r *projectRegistry) applyKVEvent(ev KVEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case KVPut:
		p, err := decodeProjectRecord(ev.Value)
		if err != nil {
			return
		}
		if prev, ok := r.byUUID[p.UUID]; ok {
			delete(r.nameIdx, prev.Name)
		}
		r.byUUID[p.UUID] = p
		r.nameIdx[p.Name] = p.UUID
	case KVDelete:
		uuid := keyToUUID(ev.Key, r.kv.Prefix())
		if prev, ok := r.byUUID[uuid]; ok {
			delete(r.byUUID, uuid)
			delete(r.nameIdx, prev.Name)
			return
		}
		if ev.PrevValue != nil {
			if prev, err := decodeProjectRecord(ev.PrevValue); err == nil {
				delete(r.byUUID, prev.UUID)
				delete(r.nameIdx, prev.Name)
			}
		}
	}
}

