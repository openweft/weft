package weft

// scripts.go — named provisioning-script catalogue. Mirror of
// flavors.go for sh-body entries an operator picks from
// CreateVMModal (weft-webui commit 690d93e).
//
// One HCL block per script, body as a multi-line string. Same
// Storage-backed pattern : file backend in dev, etcd in HA, mem in
// tests. Same exported facade so cmd/weft can hold a *ScriptRegistry
// alongside the weftServer.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// Script is one entry in the catalogue. Name is the stable key ;
// Body is the literal sh source (preserved verbatim — no
// reformatting on save). UpdatedAt / UpdatedBy are stamped by the
// RPC handler from the request context so the wire can't lie about
// provenance.
type Script struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Body        string    `json:"body"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by"`
}

// ScriptRegistry is the exported alias used by cmd/weft.
type ScriptRegistry = scriptRegistry

type scriptRegistry struct {
	mu      sync.Mutex
	storage Storage
	byName  map[string]Script
}

type scriptsDoc struct {
	Scripts []scriptBlock `hcl:"script,block"`
}
type scriptBlock struct {
	Name        string `hcl:",label"`
	Description string `hcl:"description,optional"`
	Body        string `hcl:"body"`
	UpdatedAt   string `hcl:"updated_at,optional"`
	UpdatedBy   string `hcl:"updated_by,optional"`
}

// LoadScriptRegistry reads the blob via Storage. Fresh = empty,
// same convention as the other registries.
func LoadScriptRegistry(ctx context.Context, storage Storage) (*ScriptRegistry, error) {
	reg := &scriptRegistry{
		storage: storage,
		byName:  make(map[string]Script),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load script registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc scriptsDoc
	if err := hclsimple.Decode("script-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse script registry: %w", err)
	}
	for _, b := range doc.Scripts {
		ts, _ := time.Parse(time.RFC3339Nano, b.UpdatedAt)
		reg.byName[b.Name] = Script{
			Name: b.Name, Description: b.Description, Body: b.Body,
			UpdatedAt: ts, UpdatedBy: b.UpdatedBy,
		}
	}
	return reg, nil
}

func (r *scriptRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type:  0,
		Bytes: []byte("# weft script catalogue — named POSIX sh bodies pickable from CreateVMModal.\n# Stamped onto VMs as weft.boot/script at provisioning time.\n\n"),
	}})
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := r.byName[n]
		block := body.AppendNewBlock("script", []string{n})
		bb := block.Body()
		if s.Description != "" {
			bb.SetAttributeValue("description", cty.StringVal(s.Description))
		}
		bb.SetAttributeValue("body", cty.StringVal(s.Body))
		if !s.UpdatedAt.IsZero() {
			bb.SetAttributeValue("updated_at", cty.StringVal(s.UpdatedAt.Format(time.RFC3339Nano)))
		}
		if s.UpdatedBy != "" {
			bb.SetAttributeValue("updated_by", cty.StringVal(s.UpdatedBy))
		}
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// List returns every script, sorted by name for stable callers.
func (r *scriptRegistry) List() []Script {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Script, 0, len(names))
	for _, n := range names {
		out = append(out, r.byName[n])
	}
	return out
}

func (r *scriptRegistry) Get(name string) (Script, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byName[name]
	return s, ok
}

// Set upserts a script. updatedBy is taken from the caller's
// auth context server-side ; pass "" for tests / bootstrap.
func (r *scriptRegistry) Set(s Script, updatedBy string) error {
	if s.Name == "" {
		return errors.New("script name is required")
	}
	if s.Body == "" {
		return errors.New("script body is required (empty body wouldn't run anything)")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s.UpdatedAt = time.Now().UTC()
	s.UpdatedBy = updatedBy
	r.byName[s.Name] = s
	return r.saveLocked()
}

// Delete removes a script. Idempotent.
func (r *scriptRegistry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; !ok {
		return nil
	}
	delete(r.byName, name)
	return r.saveLocked()
}
