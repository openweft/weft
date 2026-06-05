package weft

// sshkeycatalogue.go owns the cluster-wide SSH key catalogue. Each
// entry pairs a human-friendly name (unique cluster-wide) with the
// public-key blob + computed fingerprint. Distinct from the per-VM
// vm_sshkeys.go : the catalogue lives at cluster scope and VMs
// reference it by name at CreateVM time.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// SSHKeyCatalogueEntry is one entry in the cluster catalogue.
type SSHKeyCatalogueEntry struct {
	UUID        string    `json:"uuid"`
	Name        string    `json:"name"`
	PublicKey   string    `json:"public_key"`
	Fingerprint string    `json:"fingerprint"`
	Comment     string    `json:"comment,omitempty"`
	AddedAt     time.Time `json:"added_at"`
}

type sshKeyCatalogueDoc struct {
	Keys []SSHKeyCatalogueEntry `json:"keys"`
}

const sshKeyCatalogueRegistryFileName = "ssh_key_catalogue"

type sshKeyCatalogueRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]SSHKeyCatalogueEntry
	nameIdx map[string]string // name → uuid
	fpIdx   map[string]string // fingerprint → uuid (idempotent ingest)
}

func loadSSHKeyCatalogueRegistry(ctx context.Context, storage Storage) (*sshKeyCatalogueRegistry, error) {
	reg := &sshKeyCatalogueRegistry{
		storage: storage,
		byUUID:  make(map[string]SSHKeyCatalogueEntry),
		nameIdx: make(map[string]string),
		fpIdx:   make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load sshkey catalogue registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc sshKeyCatalogueDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse sshkey catalogue registry: %w", err)
	}
	for _, k := range doc.Keys {
		reg.byUUID[k.UUID] = k
		reg.nameIdx[k.Name] = k.UUID
		reg.fpIdx[k.Fingerprint] = k.UUID
	}
	return reg, nil
}

func (r *sshKeyCatalogueRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := sshKeyCatalogueDoc{Keys: make([]SSHKeyCatalogueEntry, 0, len(uuids))}
	for _, u := range uuids {
		doc.Keys = append(doc.Keys, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sshkey catalogue registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save sshkey catalogue registry: %w", err)
	}
	return nil
}

func (r *sshKeyCatalogueRegistry) lookupByUUID(uuid string) (SSHKeyCatalogueEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.byUUID[uuid]
	return k, ok
}

func (r *sshKeyCatalogueRegistry) lookupByName(name string) (SSHKeyCatalogueEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[name]
	if !ok {
		return SSHKeyCatalogueEntry{}, false
	}
	return r.byUUID[uuid], true
}

func (r *sshKeyCatalogueRegistry) list() []SSHKeyCatalogueEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SSHKeyCatalogueEntry, 0, len(r.byUUID))
	for _, k := range r.byUUID {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// add registers a new key. Idempotent on (name, fingerprint) : a
// caller re-adding the same name + key body gets the existing row.
// Conflicts on name with a different fingerprint error out.
func (r *sshKeyCatalogueRegistry) add(name, publicKey, comment string) (SSHKeyCatalogueEntry, bool, error) {
	if name == "" {
		return SSHKeyCatalogueEntry{}, false, fmt.Errorf("name is required")
	}
	if publicKey == "" {
		return SSHKeyCatalogueEntry{}, false, fmt.Errorf("public_key is required")
	}
	fp := sshKeyFingerprint(publicKey)
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.nameIdx[name]; ok {
		cur := r.byUUID[uuid]
		if cur.Fingerprint != fp {
			return SSHKeyCatalogueEntry{}, false, fmt.Errorf("name %q already used by a different key", name)
		}
		return cur, false, nil
	}
	if uuid, ok := r.fpIdx[fp]; ok {
		// Same key body under a different name : surface the existing
		// row so the caller sees the dedup happened.
		return r.byUUID[uuid], false, nil
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return SSHKeyCatalogueEntry{}, false, err
	}
	k := SSHKeyCatalogueEntry{
		UUID:        uuid,
		Name:        name,
		PublicKey:   strings.TrimSpace(publicKey),
		Fingerprint: fp,
		Comment:     comment,
		AddedAt:     time.Now().UTC(),
	}
	r.byUUID[uuid] = k
	r.nameIdx[name] = uuid
	r.fpIdx[fp] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.nameIdx, name)
		delete(r.fpIdx, fp)
		return SSHKeyCatalogueEntry{}, false, err
	}
	return k, true, nil
}

func (r *sshKeyCatalogueRegistry) remove(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("sshkey %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, cur.Name)
	delete(r.fpIdx, cur.Fingerprint)
	if err := r.saveLocked(); err != nil {
		r.byUUID[uuid] = cur
		r.nameIdx[cur.Name] = uuid
		r.fpIdx[cur.Fingerprint] = uuid
		return err
	}
	return nil
}

// importBlob walks an authorized_keys-style blob, one entry per
// non-comment line, naming each "<prefix>-<idx>" starting at 1.
// Returns the inserted rows and the duplicate-skipped count.
func (r *sshKeyCatalogueRegistry) importBlob(namePrefix, blob, comment string) ([]SSHKeyCatalogueEntry, int32, error) {
	if namePrefix == "" {
		return nil, 0, fmt.Errorf("name_prefix is required")
	}
	scanner := bufio.NewScanner(strings.NewReader(blob))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var imported []SSHKeyCatalogueEntry
	var skipped int32
	idx := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx++
		name := fmt.Sprintf("%s-%d", namePrefix, idx)
		entry, added, err := r.add(name, line, comment)
		if err != nil {
			return imported, skipped, err
		}
		if added {
			imported = append(imported, entry)
		} else {
			skipped++
		}
	}
	if err := scanner.Err(); err != nil {
		return imported, skipped, fmt.Errorf("scan blob: %w", err)
	}
	return imported, skipped, nil
}

// sshKeyFingerprint returns SHA256:<base64> over the key body
// (the middle field of "ssh-<type> <body> [comment]").
func sshKeyFingerprint(publicKey string) string {
	fields := strings.Fields(strings.TrimSpace(publicKey))
	if len(fields) < 2 {
		// Not a parseable key — fingerprint the whole blob so we
		// still get a stable identity to dedupe on.
		sum := sha256.Sum256([]byte(publicKey))
		return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	}
	body, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		sum := sha256.Sum256([]byte(publicKey))
		return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(body)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
