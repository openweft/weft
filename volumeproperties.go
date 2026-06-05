package weft

// volumeproperties.go — per-volume host-set key/value annotations.
// Mirror of vm_properties.go for block volumes ; the addressing key
// is volume_uuid rather than (vm_name, project) so the registry
// stays a flat map.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// VolumeProperty is one annotation entry.
type VolumeProperty struct {
	VolumeUUID string    `json:"volume_uuid"`
	Key        string    `json:"key"`
	Value      string    `json:"value"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type volumePropsDoc struct {
	Properties []VolumeProperty `json:"properties"`
}

const volumePropertyRegistryFileName = "volume_properties"

type volumePropertyRegistry struct {
	mu      sync.Mutex
	storage Storage
	// byVolume maps volumeUUID → key → property.
	byVolume map[string]map[string]VolumeProperty
}

func loadVolumePropertyRegistry(ctx context.Context, storage Storage) (*volumePropertyRegistry, error) {
	reg := &volumePropertyRegistry{
		storage:  storage,
		byVolume: make(map[string]map[string]VolumeProperty),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load volume-property registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc volumePropsDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse volume-property registry: %w", err)
	}
	for _, p := range doc.Properties {
		if reg.byVolume[p.VolumeUUID] == nil {
			reg.byVolume[p.VolumeUUID] = map[string]VolumeProperty{}
		}
		reg.byVolume[p.VolumeUUID][p.Key] = p
	}
	return reg, nil
}

func (r *volumePropertyRegistry) saveLocked() error {
	vols := make([]string, 0, len(r.byVolume))
	for v := range r.byVolume {
		vols = append(vols, v)
	}
	sort.Strings(vols)
	doc := volumePropsDoc{}
	for _, v := range vols {
		keys := make([]string, 0, len(r.byVolume[v]))
		for k := range r.byVolume[v] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			doc.Properties = append(doc.Properties, r.byVolume[v][k])
		}
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode volume-property registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save volume-property registry: %w", err)
	}
	return nil
}

func (r *volumePropertyRegistry) get(volumeUUID, key string) (VolumeProperty, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byVolume[volumeUUID]
	if !ok {
		return VolumeProperty{}, false
	}
	p, ok := m[key]
	return p, ok
}

func (r *volumePropertyRegistry) set(volumeUUID, key, value string) (VolumeProperty, error) {
	if volumeUUID == "" {
		return VolumeProperty{}, fmt.Errorf("volume_uuid is required")
	}
	if key == "" {
		return VolumeProperty{}, fmt.Errorf("key is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byVolume[volumeUUID] == nil {
		r.byVolume[volumeUUID] = map[string]VolumeProperty{}
	}
	p := VolumeProperty{
		VolumeUUID: volumeUUID,
		Key:        key,
		Value:      value,
		UpdatedAt:  time.Now().UTC(),
	}
	r.byVolume[volumeUUID][key] = p
	if err := r.saveLocked(); err != nil {
		return VolumeProperty{}, err
	}
	return p, nil
}

func (r *volumePropertyRegistry) delete(volumeUUID, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byVolume[volumeUUID]
	if !ok {
		return nil
	}
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	if len(m) == 0 {
		delete(r.byVolume, volumeUUID)
	}
	return r.saveLocked()
}
