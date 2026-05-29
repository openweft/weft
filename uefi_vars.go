package weft

// uefi_vars.go — per-VM UEFI NVRAM editor. Pairs with weft-webui's
// UEFI drawer tab (commit 3951da0). The hypervisor (weft-driver-vz /
// weft-driver-qemu, when wired) writes the OVMF VARS file from this
// at boot ; live edits take effect next reboot.
//
// Natural key : (vm_name, project, namespace, name). The namespace
// is the EFI vendor GUID ; an empty namespace from a wire request
// defaults to the EFI Global Variable GUID at the RPC layer.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// EFIGlobalNS is the GUID for the EFI Global Variable namespace
// (BootOrder, Boot####, SecureBoot, …). Exported because cmd/weft
// (and a future weft-cli `weft vm uefi`) reach for it when filling
// an empty wire namespace.
const EFIGlobalNS = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

// UEFIVar is one entry in the NVRAM editor. Value is hex of raw
// bytes — the byte semantics depend on the variable (uint16 LE
// list for BootOrder, UTF-16 + flags blob for Boot####, single
// byte for SecureBoot, …).
type UEFIVar struct {
	VMName     string    `json:"vm_name"`
	Project    string    `json:"project"`
	Namespace  string    `json:"namespace"` // GUID
	Name       string    `json:"name"`
	ValueHex   string    `json:"value_hex"`
	Attributes []string  `json:"attributes"` // NonVolatile, BootServiceAccess, ...
	UpdatedAt  time.Time `json:"updated_at"`
}

// UEFIVarRegistry is the exported alias for cmd/weft.
type UEFIVarRegistry = uefiVarRegistry

type uefiVarRegistry struct {
	mu      sync.Mutex
	storage Storage
	// byVM keys "<project>/<vm>" → "<ns>/<name>" → entry.
	byVM map[string]map[string]UEFIVar
}

type uefiVarsDoc struct {
	Entries []uefiVarBlock `hcl:"uefi_var,block"`
}
type uefiVarBlock struct {
	// Label = "<project>/<vm>/<namespace>/<name>" for uniqueness.
	ID         string   `hcl:",label"`
	VMName     string   `hcl:"vm_name"`
	Project    string   `hcl:"project"`
	Namespace  string   `hcl:"namespace"`
	Name       string   `hcl:"name"`
	ValueHex   string   `hcl:"value_hex"`
	Attributes []string `hcl:"attributes,optional"`
	UpdatedAt  string   `hcl:"updated_at,optional"`
}

// LoadUEFIVarRegistry reads the blob via Storage.
func LoadUEFIVarRegistry(ctx context.Context, storage Storage) (*UEFIVarRegistry, error) {
	reg := &uefiVarRegistry{
		storage: storage,
		byVM:    make(map[string]map[string]UEFIVar),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load uefi-var registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc uefiVarsDoc
	if err := hclsimple.Decode("uefi-vars.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse uefi-var registry: %w", err)
	}
	for _, e := range doc.Entries {
		ts, _ := time.Parse(time.RFC3339Nano, e.UpdatedAt)
		scope := vmScopeKey(e.Project, e.VMName)
		varKey := e.Namespace + "/" + e.Name
		if reg.byVM[scope] == nil {
			reg.byVM[scope] = map[string]UEFIVar{}
		}
		reg.byVM[scope][varKey] = UEFIVar{
			VMName: e.VMName, Project: e.Project,
			Namespace: e.Namespace, Name: e.Name,
			ValueHex: e.ValueHex, Attributes: append([]string(nil), e.Attributes...),
			UpdatedAt: ts,
		}
	}
	return reg, nil
}

func (r *uefiVarRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type:  0,
		Bytes: []byte("# weft UEFI variables — per-VM NVRAM editor.\n# Block labels combine <project>/<vm>/<namespace>/<name> for uniqueness.\n# Values are hex of the raw byte blob ; attributes follow the standard\n# UEFI flag set (NonVolatile, BootServiceAccess, RuntimeAccess, ...).\n\n"),
	}})
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
			v := r.byVM[sc][k]
			label := v.Project + "/" + v.VMName + "/" + v.Namespace + "/" + v.Name
			block := body.AppendNewBlock("uefi_var", []string{label})
			bb := block.Body()
			bb.SetAttributeValue("vm_name", cty.StringVal(v.VMName))
			bb.SetAttributeValue("project", cty.StringVal(v.Project))
			bb.SetAttributeValue("namespace", cty.StringVal(v.Namespace))
			bb.SetAttributeValue("name", cty.StringVal(v.Name))
			bb.SetAttributeValue("value_hex", cty.StringVal(v.ValueHex))
			if len(v.Attributes) > 0 {
				vals := make([]cty.Value, len(v.Attributes))
				for i, a := range v.Attributes {
					vals[i] = cty.StringVal(a)
				}
				bb.SetAttributeValue("attributes", cty.ListVal(vals))
			}
			if !v.UpdatedAt.IsZero() {
				bb.SetAttributeValue("updated_at", cty.StringVal(v.UpdatedAt.Format(time.RFC3339Nano)))
			}
			body.AppendNewline()
		}
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// ListForVM returns every UEFI var attached to (project, vmName),
// sorted by (namespace, name) for diff-friendly output.
func (r *uefiVarRegistry) ListForVM(project, vmName string) []UEFIVar {
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
	out := make([]UEFIVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// Set upserts a UEFI variable. Validates the hex shape + defaults
// empty namespace to EFIGlobalNS.
func (r *uefiVarRegistry) Set(v UEFIVar) error {
	if v.VMName == "" {
		return errors.New("vm_name is required")
	}
	if v.Name == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(v.Namespace) == "" {
		v.Namespace = EFIGlobalNS
	}
	// Strip whitespace from value_hex (operator may paste with
	// readability spaces) before validating.
	v.ValueHex = strings.ReplaceAll(strings.TrimSpace(v.ValueHex), " ", "")
	if !validHex(v.ValueHex) {
		return errors.New("value_hex must be a (possibly empty) sequence of hex pairs")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v.UpdatedAt = time.Now().UTC()
	scope := vmScopeKey(v.Project, v.VMName)
	if r.byVM[scope] == nil {
		r.byVM[scope] = map[string]UEFIVar{}
	}
	r.byVM[scope][v.Namespace+"/"+v.Name] = v
	return r.saveLocked()
}

// Delete removes one entry. Idempotent.
func (r *uefiVarRegistry) Delete(project, vmName, namespace, name string) error {
	if strings.TrimSpace(namespace) == "" {
		namespace = EFIGlobalNS
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	scope := vmScopeKey(project, vmName)
	m, ok := r.byVM[scope]
	if !ok {
		return nil
	}
	varKey := namespace + "/" + name
	if _, ok := m[varKey]; !ok {
		return nil
	}
	delete(m, varKey)
	if len(m) == 0 {
		delete(r.byVM, scope)
	}
	return r.saveLocked()
}

// validHex returns true when s is empty OR an even-length hex
// sequence. Empty is valid — a UEFI variable can carry an empty
// value.
func validHex(s string) bool {
	if s == "" {
		return true
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
