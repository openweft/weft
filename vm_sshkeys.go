package weft

// vm_sshkeys.go — per-VM SSH-key store. AddVMSSHKey takes a raw
// OpenSSH-format public-key line ; the server parses it, computes
// the SHA256 fingerprint, stores. The fingerprint is the stable
// identity for RemoveVMSSHKey (operators don't URL-encode 400 bytes
// of base64).
//
// Pairs with weft-webui's per-VM SSH-keys path (commit e5ac6b9 +
// 9ed80b9) and the in-guest weft-microvm-agent pkg/sshkeys subscriber
// (commit 032f346) + the embedded sshd (commit b6c20af).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

// VMSSHKey is one authorised SSH key on a VM. Fingerprint is
// SHA256:<b64> ; same shape weft-webui's drawer + the guest's
// pkg/sshkeys ParseAuthorizedKey produces.
type VMSSHKey struct {
	VMName      string    `json:"vm_name"`
	Project     string    `json:"project"`
	Fingerprint string    `json:"fingerprint"`
	Type        string    `json:"type"`       // "ssh-ed25519" | "ssh-rsa" | ...
	PublicKey   string    `json:"public_key"` // full "<type> <b64> [comment]" line
	Comment     string    `json:"comment"`
	AddedAt     time.Time `json:"added_at"`
}

// VMSSHKeyRegistry is the exported alias for cmd/weft.
type VMSSHKeyRegistry = vmSSHKeyRegistry

type vmSSHKeyRegistry struct {
	mu      sync.Mutex
	storage Storage
	// byVM maps "<project>/<vm>" → fingerprint → entry.
	byVM map[string]map[string]VMSSHKey
}

type vmSSHKeysDoc struct {
	Entries []vmSSHKeyBlock `hcl:"ssh_key,block"`
}
type vmSSHKeyBlock struct {
	// Label = "<project>/<vm>/<fingerprint>" — fingerprint is
	// already base64-safe + uniquely identifies the key.
	ID          string `hcl:",label"`
	VMName      string `hcl:"vm_name"`
	Project     string `hcl:"project"`
	Fingerprint string `hcl:"fingerprint"`
	Type        string `hcl:"type"`
	PublicKey   string `hcl:"public_key"`
	Comment     string `hcl:"comment,optional"`
	AddedAt     string `hcl:"added_at,optional"`
}

// LoadVMSSHKeyRegistry reads the blob via Storage.
func LoadVMSSHKeyRegistry(ctx context.Context, storage Storage) (*VMSSHKeyRegistry, error) {
	reg := &vmSSHKeyRegistry{
		storage: storage,
		byVM:    make(map[string]map[string]VMSSHKey),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load vm-sshkey registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc vmSSHKeysDoc
	if err := hclsimple.Decode("vm-sshkeys.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse vm-sshkey registry: %w", err)
	}
	for _, e := range doc.Entries {
		ts, _ := time.Parse(time.RFC3339Nano, e.AddedAt)
		scope := vmScopeKey(e.Project, e.VMName)
		if reg.byVM[scope] == nil {
			reg.byVM[scope] = map[string]VMSSHKey{}
		}
		reg.byVM[scope][e.Fingerprint] = VMSSHKey{
			VMName: e.VMName, Project: e.Project,
			Fingerprint: e.Fingerprint, Type: e.Type,
			PublicKey: e.PublicKey, Comment: e.Comment, AddedAt: ts,
		}
	}
	return reg, nil
}

func (r *vmSSHKeyRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type:  0,
		Bytes: []byte("# weft per-VM SSH keys — authorised keys for each VM.\n# Operators add via the dashboard's SSH-keys tab + import from gh/gl/forgejo.\n# Fingerprints (SHA256:<b64>) are server-computed identity.\n\n"),
	}})
	scopes := make([]string, 0, len(r.byVM))
	for s := range r.byVM {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	for _, sc := range scopes {
		fps := make([]string, 0, len(r.byVM[sc]))
		for fp := range r.byVM[sc] {
			fps = append(fps, fp)
		}
		sort.Strings(fps)
		for _, fp := range fps {
			k := r.byVM[sc][fp]
			label := k.Project + "/" + k.VMName + "/" + k.Fingerprint
			block := body.AppendNewBlock("ssh_key", []string{label})
			bb := block.Body()
			bb.SetAttributeValue("vm_name", cty.StringVal(k.VMName))
			bb.SetAttributeValue("project", cty.StringVal(k.Project))
			bb.SetAttributeValue("fingerprint", cty.StringVal(k.Fingerprint))
			bb.SetAttributeValue("type", cty.StringVal(k.Type))
			bb.SetAttributeValue("public_key", cty.StringVal(k.PublicKey))
			if k.Comment != "" {
				bb.SetAttributeValue("comment", cty.StringVal(k.Comment))
			}
			if !k.AddedAt.IsZero() {
				bb.SetAttributeValue("added_at", cty.StringVal(k.AddedAt.Format(time.RFC3339Nano)))
			}
			body.AppendNewline()
		}
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// ListForVM returns the authorised keys for (project, vmName)
// sorted by fingerprint (stable callers).
func (r *vmSSHKeyRegistry) ListForVM(project, vmName string) []VMSSHKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.byVM[vmScopeKey(project, vmName)]
	if len(m) == 0 {
		return nil
	}
	fps := make([]string, 0, len(m))
	for fp := range m {
		fps = append(fps, fp)
	}
	sort.Strings(fps)
	out := make([]VMSSHKey, 0, len(fps))
	for _, fp := range fps {
		out = append(out, m[fp])
	}
	return out
}

// Add parses the OpenSSH-format publicKey, computes the SHA256
// fingerprint, stores the entry. Idempotent on fingerprint
// (re-add returns the existing entry without duplication).
// Returns the stored entry (with the server-stamped AddedAt +
// resolved Type/Comment) so the caller doesn't re-parse.
func (r *vmSSHKeyRegistry) Add(project, vmName, publicKey string) (VMSSHKey, error) {
	if vmName == "" {
		return VMSSHKey{}, errors.New("vm_name is required")
	}
	typ, comment, fp, ok := parseSSHLine(publicKey)
	if !ok {
		return VMSSHKey{}, errors.New("public_key must be '<type> <base64> [comment]' with a known algorithm")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	scope := vmScopeKey(project, vmName)
	if existing, ok := r.byVM[scope][fp]; ok {
		// Idempotent — same fingerprint already stored, return as-is.
		return existing, nil
	}
	if r.byVM[scope] == nil {
		r.byVM[scope] = map[string]VMSSHKey{}
	}
	entry := VMSSHKey{
		VMName: vmName, Project: project,
		Fingerprint: fp, Type: typ,
		PublicKey: strings.TrimSpace(publicKey),
		Comment:   comment, AddedAt: time.Now().UTC(),
	}
	r.byVM[scope][fp] = entry
	if err := r.saveLocked(); err != nil {
		return VMSSHKey{}, err
	}
	return entry, nil
}

// Remove deletes by fingerprint. Idempotent.
func (r *vmSSHKeyRegistry) Remove(project, vmName, fingerprint string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	scope := vmScopeKey(project, vmName)
	m, ok := r.byVM[scope]
	if !ok {
		return nil
	}
	if _, ok := m[fingerprint]; !ok {
		return nil
	}
	delete(m, fingerprint)
	if len(m) == 0 {
		delete(r.byVM, scope)
	}
	return r.saveLocked()
}

// validSSHKeyTypes is the closed type set the registry accepts.
// Same vocabulary the in-guest pkg/sshkeys validator uses.
var validSSHKeyTypes = map[string]bool{
	"ssh-rsa": true, "ssh-ed25519": true, "ssh-dss": true,
	"ecdsa-sha2-nistp256": true,
	"ecdsa-sha2-nistp384": true,
	"ecdsa-sha2-nistp521": true,
}

// parseSSHLine validates "<type> <base64> [comment]" and returns
// the broken-out fields + SHA256:<b64> fingerprint. ok=false on
// any shape problem.
func parseSSHLine(line string) (typ, comment, fingerprint string, ok bool) {
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) < 2 {
		return "", "", "", false
	}
	if !validSSHKeyTypes[parts[0]] {
		return "", "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", "", false
	}
	sum := sha256.Sum256(raw)
	fp := "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
	c := ""
	if len(parts) > 2 {
		c = strings.Join(parts[2:], " ")
	}
	return parts[0], c, fp, true
}
