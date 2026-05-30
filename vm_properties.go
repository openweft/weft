package weft

// vm_properties.go — per-VM host-set key/value annotations. Pairs
// with weft-webui's drawer Properties tab (commit 3951da0) +
// weft-microvm-agent's pkg/properties subscriber (commit bbaec47) which
// mirrors guest_readable entries to /run/weft/properties/.
//
// Storage shape : single blob holding the whole map[(vm_name,
// project)][]VMProperty. One HCL file (or one etcd key) for the
// whole cluster ; scales fine at the catalogue cardinality this
// installation realistically sees (per-VM rows × few props each).
// A future per-VM blob split is a one-file refactor when the row
// count gets uncomfortable.
//
// The reserved "weft.boot/*" prefix is owned by the first-boot
// provisioning path (lifecycle.go writeBootProperty in weft-webui) ;
// operators may read but should not hand-edit those values.

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

// VMProperty is one annotation entry. The (VMName, Project, Key)
// triple is the natural key — VM names aren't globally unique, so
// project narrows the address.
type VMProperty struct {
	VMName        string    `json:"vm_name"`
	Project       string    `json:"project"`
	Key           string    `json:"key"`
	Value         string    `json:"value"`
	GuestReadable bool      `json:"guest_readable"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// VMPropertyRegistry is the exported alias for cmd/weft.
type VMPropertyRegistry = vmPropertyRegistry

type vmPropertyRegistry struct {
	mu      sync.Mutex
	storage Storage
	// byVM maps "<project>/<vm_name>" → key → property. Two-level
	// indirection because callers list-by-VM exclusively, and the
	// first-level key is small (project + name).
	byVM map[string]map[string]VMProperty
}

// scope is the addressing tuple (project, vm). Internal helper —
// callers pass project + vmName separately.
func vmScopeKey(project, vmName string) string {
	return project + "/" + vmName
}

type vmPropsDoc struct {
	Entries []vmPropBlock `hcl:"property,block"`
}
type vmPropBlock struct {
	// Block label combines project + vm + key as "<project>/<vm>/<key>"
	// so the label is unique within the file.
	ID            string `hcl:",label"`
	VMName        string `hcl:"vm_name"`
	Project       string `hcl:"project"`
	Key           string `hcl:"key"`
	Value         string `hcl:"value"`
	GuestReadable bool   `hcl:"guest_readable,optional"`
	UpdatedAt     string `hcl:"updated_at,optional"`
}

// LoadVMPropertyRegistry reads the blob via Storage. Fresh = empty.
func LoadVMPropertyRegistry(ctx context.Context, storage Storage) (*VMPropertyRegistry, error) {
	reg := &vmPropertyRegistry{
		storage: storage,
		byVM:    make(map[string]map[string]VMProperty),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load vm-property registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc vmPropsDoc
	if err := hclsimple.Decode("vm-properties.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse vm-property registry: %w", err)
	}
	for _, e := range doc.Entries {
		ts, _ := time.Parse(time.RFC3339Nano, e.UpdatedAt)
		scope := vmScopeKey(e.Project, e.VMName)
		if reg.byVM[scope] == nil {
			reg.byVM[scope] = map[string]VMProperty{}
		}
		reg.byVM[scope][e.Key] = VMProperty{
			VMName: e.VMName, Project: e.Project, Key: e.Key,
			Value: e.Value, GuestReadable: e.GuestReadable, UpdatedAt: ts,
		}
	}
	return reg, nil
}

func (r *vmPropertyRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type:  0,
		Bytes: []byte("# weft per-VM properties — host-set annotations on each VM.\n# guest_readable=true entries flow to the in-guest weft-microvm-agent via NATS.\n# Block labels combine <project>/<vm>/<key> for uniqueness ; the body\n# carries the canonical fields.\n\n"),
	}})
	// Stable sort by (project, vm, key) for diff-friendly output.
	scopes := make([]string, 0, len(r.byVM))
	for s := range r.byVM {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	for _, sc := range scopes {
		keys := make([]string, 0, len(r.byVM[sc]))
		for k := range r.byVM[sc] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := r.byVM[sc][k]
			label := p.Project + "/" + p.VMName + "/" + p.Key
			block := body.AppendNewBlock("property", []string{label})
			bb := block.Body()
			bb.SetAttributeValue("vm_name", cty.StringVal(p.VMName))
			bb.SetAttributeValue("project", cty.StringVal(p.Project))
			bb.SetAttributeValue("key", cty.StringVal(p.Key))
			bb.SetAttributeValue("value", cty.StringVal(p.Value))
			if p.GuestReadable {
				bb.SetAttributeValue("guest_readable", cty.True)
			}
			if !p.UpdatedAt.IsZero() {
				bb.SetAttributeValue("updated_at", cty.StringVal(p.UpdatedAt.Format(time.RFC3339Nano)))
			}
			body.AppendNewline()
		}
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// ListForVM returns every property attached to (project, vmName),
// sorted by key. Empty list when the VM has none.
func (r *vmPropertyRegistry) ListForVM(project, vmName string) []VMProperty {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.byVM[vmScopeKey(project, vmName)]
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]VMProperty, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// Set upserts a property. UpdatedAt is stamped here.
func (r *vmPropertyRegistry) Set(p VMProperty) error {
	if p.VMName == "" {
		return errors.New("vm_name is required")
	}
	if p.Key == "" {
		return errors.New("key is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p.UpdatedAt = time.Now().UTC()
	scope := vmScopeKey(p.Project, p.VMName)
	if r.byVM[scope] == nil {
		r.byVM[scope] = map[string]VMProperty{}
	}
	r.byVM[scope][p.Key] = p
	return r.saveLocked()
}

// Delete removes one property. Idempotent.
func (r *vmPropertyRegistry) Delete(project, vmName, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	scope := vmScopeKey(project, vmName)
	m, ok := r.byVM[scope]
	if !ok {
		return nil
	}
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	if len(m) == 0 {
		delete(r.byVM, scope)
	}
	return r.saveLocked()
}
