package weft

// shares.go owns the Share registry — per-project CubeFS share
// catalogue (v0.8 added the proto, v0.9 adds the server-side wiring +
// Get/Resize extensions). Shares are control-plane catalogue entries ;
// the actual mount fan-out lives in share.go (PublishShareToProject).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Share is one entry in the registry.
type Share struct {
	UUID        string    `json:"uuid"`
	Name        string    `json:"name"`
	ProjectUUID string    `json:"project_uuid"`
	Backend     string    `json:"backend"` // "cubefs"
	SizeGB      int64     `json:"size_gb"`
	Readonly    bool      `json:"readonly"`
	Status      string    `json:"status"` // "provisioning" | "active" | "failed"
	CreatedAt   time.Time `json:"created_at"`
}

type sharesDoc struct {
	Shares []Share `json:"shares"`
}

const shareRegistryFileName = "shares"

type shareRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]Share
	// nameIdx maps "<project>/<name>" → uuid for the per-project
	// uniqueness check on Create.
	nameIdx map[string]string
}

func loadShareRegistry(ctx context.Context, storage Storage) (*shareRegistry, error) {
	reg := &shareRegistry{
		storage: storage,
		byUUID:  make(map[string]Share),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load share registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc sharesDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse share registry: %w", err)
	}
	for _, s := range doc.Shares {
		reg.byUUID[s.UUID] = s
		reg.nameIdx[s.ProjectUUID+"/"+s.Name] = s.UUID
	}
	return reg, nil
}

func (r *shareRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := sharesDoc{Shares: make([]Share, 0, len(uuids))}
	for _, u := range uuids {
		doc.Shares = append(doc.Shares, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode share registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save share registry: %w", err)
	}
	return nil
}

func (r *shareRegistry) lookupByUUID(uuid string) (Share, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byUUID[uuid]
	return s, ok
}

func (r *shareRegistry) list(projectUUID string) []Share {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Share, 0, len(r.byUUID))
	for _, s := range r.byUUID {
		if projectUUID != "" && s.ProjectUUID != projectUUID {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (r *shareRegistry) create(projectUUID, name string, sizeGB int64, readonly bool, backend string) (Share, bool, error) {
	if projectUUID == "" {
		return Share{}, false, fmt.Errorf("project_uuid is required")
	}
	if name == "" {
		return Share{}, false, fmt.Errorf("name is required")
	}
	if sizeGB <= 0 {
		return Share{}, false, fmt.Errorf("size_gb must be > 0")
	}
	if backend == "" {
		backend = "cubefs"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := projectUUID + "/" + name
	if uuid, ok := r.nameIdx[key]; ok {
		return r.byUUID[uuid], false, nil
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return Share{}, false, err
	}
	s := Share{
		UUID:        uuid,
		Name:        name,
		ProjectUUID: projectUUID,
		Backend:     backend,
		SizeGB:      sizeGB,
		Readonly:    readonly,
		Status:      "provisioning",
		CreatedAt:   time.Now().UTC(),
	}
	r.byUUID[uuid] = s
	r.nameIdx[key] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.nameIdx, key)
		return Share{}, false, err
	}
	return s, true, nil
}

// resize bumps SizeGB. Refuses to shrink (quota mutation is
// monotonic up — CubeFS can't shrink an in-use volume safely).
func (r *shareRegistry) resize(uuid string, newSizeGB int64) (Share, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return Share{}, fmt.Errorf("share %s not found", uuid)
	}
	if newSizeGB <= cur.SizeGB {
		return Share{}, fmt.Errorf("new_size_gb must be > current %d GB", cur.SizeGB)
	}
	cur.SizeGB = newSizeGB
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return Share{}, err
	}
	return cur, nil
}

func (r *shareRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("share %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, cur.ProjectUUID+"/"+cur.Name)
	if err := r.saveLocked(); err != nil {
		r.byUUID[uuid] = cur
		r.nameIdx[cur.ProjectUUID+"/"+cur.Name] = uuid
		return err
	}
	return nil
}
