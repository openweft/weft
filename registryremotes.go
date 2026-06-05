package weft

// registryremotes.go owns the OCI registry alias catalogue. Each
// row pairs a human-friendly name ("ghcr", "mirror") with the
// endpoint + insecure flag + credential_secret_ref the OCI puller
// reaches when resolving an alias.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// RegistryRemote is one entry in the registry.
type RegistryRemote struct {
	UUID                string    `json:"uuid"`
	Name                string    `json:"name"`
	Endpoint            string    `json:"endpoint"`
	Insecure            bool      `json:"insecure,omitempty"`
	CredentialSecretRef string    `json:"credential_secret_ref,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

type registryRemotesDoc struct {
	Remotes []RegistryRemote `json:"remotes"`
}

const registryRemoteRegistryFileName = "registry_remotes"

type registryRemoteRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]RegistryRemote
	nameIdx map[string]string // name → uuid
}

func loadRegistryRemoteRegistry(ctx context.Context, storage Storage) (*registryRemoteRegistry, error) {
	reg := &registryRemoteRegistry{
		storage: storage,
		byUUID:  make(map[string]RegistryRemote),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load registry remote registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc registryRemotesDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse registry remote registry: %w", err)
	}
	for _, rr := range doc.Remotes {
		reg.byUUID[rr.UUID] = rr
		reg.nameIdx[rr.Name] = rr.UUID
	}
	return reg, nil
}

func (r *registryRemoteRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := registryRemotesDoc{Remotes: make([]RegistryRemote, 0, len(uuids))}
	for _, u := range uuids {
		doc.Remotes = append(doc.Remotes, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry remote registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save registry remote registry: %w", err)
	}
	return nil
}

func (r *registryRemoteRegistry) lookupByUUID(uuid string) (RegistryRemote, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr, ok := r.byUUID[uuid]
	return rr, ok
}

func (r *registryRemoteRegistry) lookupByName(name string) (RegistryRemote, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[name]
	if !ok {
		return RegistryRemote{}, false
	}
	return r.byUUID[uuid], true
}

func (r *registryRemoteRegistry) list() []RegistryRemote {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RegistryRemote, 0, len(r.byUUID))
	for _, rr := range r.byUUID {
		out = append(out, rr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// set upserts by name. Empty endpoint on an existing row = keep
// current endpoint (partial PATCH on alias updates).
func (r *registryRemoteRegistry) set(name, endpoint string, insecure bool, credSecretRef string) (RegistryRemote, bool, error) {
	if name == "" {
		return RegistryRemote{}, false, fmt.Errorf("name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.nameIdx[name]; ok {
		cur := r.byUUID[uuid]
		if endpoint != "" {
			cur.Endpoint = endpoint
		}
		cur.Insecure = insecure
		if credSecretRef != "" {
			cur.CredentialSecretRef = credSecretRef
		}
		r.byUUID[uuid] = cur
		if err := r.saveLocked(); err != nil {
			return RegistryRemote{}, false, err
		}
		return cur, false, nil
	}
	if endpoint == "" {
		return RegistryRemote{}, false, fmt.Errorf("endpoint is required for new remote")
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return RegistryRemote{}, false, err
	}
	rr := RegistryRemote{
		UUID:                uuid,
		Name:                name,
		Endpoint:            endpoint,
		Insecure:            insecure,
		CredentialSecretRef: credSecretRef,
		CreatedAt:           time.Now().UTC(),
	}
	r.byUUID[uuid] = rr
	r.nameIdx[name] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.nameIdx, name)
		return RegistryRemote{}, false, err
	}
	return rr, true, nil
}

func (r *registryRemoteRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("registry remote %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, cur.Name)
	if err := r.saveLocked(); err != nil {
		r.byUUID[uuid] = cur
		r.nameIdx[cur.Name] = uuid
		return err
	}
	return nil
}
