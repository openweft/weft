package weft

// buckets.go owns the Bucket registry — per-project S3 bucket
// catalogue. The actual object data lives on the S3 endpoint
// (versitygw or CubeFS objectnode) ; the openweft registry tracks
// the credential bundle + a mutable policy JSON the operator
// rotates via SetBucketPolicy.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Bucket is one entry in the registry.
type Bucket struct {
	UUID            string    `json:"uuid"`
	ProjectUUID     string    `json:"project_uuid"`
	Name            string    `json:"name"`
	Endpoint        string    `json:"endpoint"`
	Region          string    `json:"region,omitempty"`
	AccessKeyID     string    `json:"access_key_id,omitempty"`
	SecretAccessKey string    `json:"secret_access_key,omitempty"`
	Policy          string    `json:"policy,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type bucketsDoc struct {
	Buckets []Bucket `json:"buckets"`
}

const bucketRegistryFileName = "buckets"

type bucketRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]Bucket
	// nameIdx maps "<project>/<name>" → uuid.
	nameIdx map[string]string
}

func loadBucketRegistry(ctx context.Context, storage Storage) (*bucketRegistry, error) {
	reg := &bucketRegistry{
		storage: storage,
		byUUID:  make(map[string]Bucket),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load bucket registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc bucketsDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse bucket registry: %w", err)
	}
	for _, b := range doc.Buckets {
		reg.byUUID[b.UUID] = b
		reg.nameIdx[b.ProjectUUID+"/"+b.Name] = b.UUID
	}
	return reg, nil
}

func (r *bucketRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := bucketsDoc{Buckets: make([]Bucket, 0, len(uuids))}
	for _, u := range uuids {
		doc.Buckets = append(doc.Buckets, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode bucket registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save bucket registry: %w", err)
	}
	return nil
}

func (r *bucketRegistry) lookupByUUID(uuid string) (Bucket, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.byUUID[uuid]
	return b, ok
}

func (r *bucketRegistry) list(projectUUID string) []Bucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Bucket, 0, len(r.byUUID))
	for _, b := range r.byUUID {
		if projectUUID != "" && b.ProjectUUID != projectUUID {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (r *bucketRegistry) create(projectUUID, name, endpoint, region, accessKey, secretKey string) (Bucket, bool, error) {
	if projectUUID == "" {
		return Bucket{}, false, fmt.Errorf("project_uuid is required")
	}
	if name == "" {
		return Bucket{}, false, fmt.Errorf("name is required")
	}
	if endpoint == "" {
		return Bucket{}, false, fmt.Errorf("endpoint is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := projectUUID + "/" + name
	if uuid, ok := r.nameIdx[key]; ok {
		return r.byUUID[uuid], false, nil
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return Bucket{}, false, err
	}
	b := Bucket{
		UUID:            uuid,
		ProjectUUID:     projectUUID,
		Name:            name,
		Endpoint:        endpoint,
		Region:          region,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		CreatedAt:       time.Now().UTC(),
	}
	r.byUUID[uuid] = b
	r.nameIdx[key] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.nameIdx, key)
		return Bucket{}, false, err
	}
	return b, true, nil
}

func (r *bucketRegistry) setPolicy(uuid, policy string) (Bucket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return Bucket{}, fmt.Errorf("bucket %s not found", uuid)
	}
	cur.Policy = policy
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return Bucket{}, err
	}
	return cur, nil
}

func (r *bucketRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("bucket %s not found", uuid)
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
