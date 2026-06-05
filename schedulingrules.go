package weft

// schedulingrules.go owns the SchedulingRule registry — cluster-wide
// nominal placement rules per [[openweft_nominal_binding]]. A rule
// pairs a label selector with a target_count + anti-affinity domain ;
// the scheduler reconciles toward target_count VMs matching the
// selector (or pinned by nominal binding on the VM record).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SchedulingRuleEntry is one entry in the registry. The name is
// "Entry" suffix to avoid collision with the in-tree scheduler types.
type SchedulingRuleEntry struct {
	UUID         string    `json:"uuid"`
	Name         string    `json:"name"`
	Selector     string    `json:"selector"` // comma-separated "k=v" pairs
	TargetCount  int32     `json:"target_count"`
	AntiAffinity string    `json:"anti_affinity,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type schedulingRulesDoc struct {
	Rules []SchedulingRuleEntry `json:"rules"`
}

const schedulingRuleRegistryFileName = "scheduling_rules"

type schedulingRuleRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]SchedulingRuleEntry
	nameIdx map[string]string // name → uuid
}

func loadSchedulingRuleRegistry(ctx context.Context, storage Storage) (*schedulingRuleRegistry, error) {
	reg := &schedulingRuleRegistry{
		storage: storage,
		byUUID:  make(map[string]SchedulingRuleEntry),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load scheduling rule registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc schedulingRulesDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse scheduling rule registry: %w", err)
	}
	for _, sr := range doc.Rules {
		reg.byUUID[sr.UUID] = sr
		reg.nameIdx[sr.Name] = sr.UUID
	}
	return reg, nil
}

func (r *schedulingRuleRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := schedulingRulesDoc{Rules: make([]SchedulingRuleEntry, 0, len(uuids))}
	for _, u := range uuids {
		doc.Rules = append(doc.Rules, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode scheduling rule registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save scheduling rule registry: %w", err)
	}
	return nil
}

func (r *schedulingRuleRegistry) lookupByUUID(uuid string) (SchedulingRuleEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sr, ok := r.byUUID[uuid]
	return sr, ok
}

func (r *schedulingRuleRegistry) list() []SchedulingRuleEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SchedulingRuleEntry, 0, len(r.byUUID))
	for _, sr := range r.byUUID {
		out = append(out, sr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *schedulingRuleRegistry) create(name, selector string, targetCount int32, antiAffinity string) (SchedulingRuleEntry, bool, error) {
	if name == "" {
		return SchedulingRuleEntry{}, false, fmt.Errorf("name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.nameIdx[name]; ok {
		return r.byUUID[uuid], false, nil
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return SchedulingRuleEntry{}, false, err
	}
	sr := SchedulingRuleEntry{
		UUID:         uuid,
		Name:         name,
		Selector:     selector,
		TargetCount:  targetCount,
		AntiAffinity: antiAffinity,
		CreatedAt:    time.Now().UTC(),
	}
	r.byUUID[uuid] = sr
	r.nameIdx[name] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.nameIdx, name)
		return SchedulingRuleEntry{}, false, err
	}
	return sr, true, nil
}

// update patches the mutable fields. Empty selector / antiAffinity
// = keep current ; targetCount == -1 = keep current.
func (r *schedulingRuleRegistry) update(uuid, selector string, targetCount int32, antiAffinity string) (SchedulingRuleEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return SchedulingRuleEntry{}, fmt.Errorf("scheduling rule %s not found", uuid)
	}
	if selector != "" {
		cur.Selector = selector
	}
	if targetCount >= 0 {
		cur.TargetCount = targetCount
	}
	if antiAffinity != "" {
		cur.AntiAffinity = antiAffinity
	}
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return SchedulingRuleEntry{}, err
	}
	return cur, nil
}

func (r *schedulingRuleRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("scheduling rule %s not found", uuid)
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
