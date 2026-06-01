package pluginstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Instance is the persistent record of one installed plugin instance.
// Multiple instances of the same plugin can coexist as long as their
// Project differs.
type Instance struct {
	Name        string            `json:"name"`         // plugin name (manifest.Name)
	UUID        string            `json:"uuid"`         // instance UUID — stable across reruns
	Version     string            `json:"version"`      // manifest version at install time
	Project     string            `json:"project"`      // weft project name or UUID
	InstalledAt time.Time         `json:"installed_at"` // wallclock
	Inputs      map[string]string `json:"inputs"`       // resolved inputs (secrets blanked)

	// Resource handles created at install time. Stored so
	// Uninstall can tear them down in reverse order without
	// re-deriving names.
	Networks       []string `json:"networks"`        // UUIDs
	SecurityGroups []string `json:"security_groups"` // UUIDs
	VMs            []string `json:"vms"`             // names (project-scoped)
	Volumes        []string `json:"volumes"`         // UUIDs
}

// StateStore persists the catalogue's installed-instance records.
// Real deployments back this with etcd (under /weft/plugins/…) ; the
// dev / test default is a JSON file under the agent's state dir.
type StateStore interface {
	List() ([]Instance, error)
	Get(name, uuid string) (Instance, bool, error)
	Put(Instance) error
	Delete(name, uuid string) error
}

// ---------------------------------------------------------------
// In-memory implementation — used by tests and the file store's
// in-process cache.
// ---------------------------------------------------------------

type MemStore struct {
	mu    sync.Mutex
	items map[string]Instance // key = name + "/" + uuid
}

func NewMemStore() *MemStore { return &MemStore{items: map[string]Instance{}} }

func (m *MemStore) key(name, uuid string) string { return name + "/" + uuid }

func (m *MemStore) List() ([]Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Instance, 0, len(m.items))
	for _, v := range m.items {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].UUID < out[j].UUID
	})
	return out, nil
}

func (m *MemStore) Get(name, uuid string) (Instance, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[m.key(name, uuid)]
	return v, ok, nil
}

func (m *MemStore) Put(in Instance) error {
	if in.Name == "" || in.UUID == "" {
		return errors.New("MemStore.Put: name and uuid required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[m.key(in.Name, in.UUID)] = in
	return nil
}

func (m *MemStore) Delete(name, uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, m.key(name, uuid))
	return nil
}

// ---------------------------------------------------------------
// File-backed implementation — JSON file per instance. Lives under
// <stateDir>/plugins/<name>/<uuid>.json. The agent's etcd-backed
// adapter mirrors this layout under /weft/plugins/<name>/<uuid> when
// running on etcd (per the openweft-etcd-embedded memory).
// ---------------------------------------------------------------

type FileStore struct {
	mu  sync.Mutex
	dir string
}

// NewFileStore returns a FileStore rooted at <dir>. Parent dirs are
// created on first Put.
func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

func (f *FileStore) path(name, uuid string) string {
	return filepath.Join(f.dir, name, uuid+".json")
}

func (f *FileStore) Put(in Instance) error {
	if in.Name == "" || in.UUID == "" {
		return errors.New("FileStore.Put: name and uuid required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.path(in.Name, in.UUID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(p), err)
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (f *FileStore) Get(name, uuid string) (Instance, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path(name, uuid))
	if errors.Is(err, os.ErrNotExist) {
		return Instance{}, false, nil
	}
	if err != nil {
		return Instance{}, false, err
	}
	var in Instance
	if err := json.Unmarshal(data, &in); err != nil {
		return Instance{}, false, err
	}
	return in, true, nil
}

func (f *FileStore) Delete(name, uuid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(name, uuid))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (f *FileStore) List() ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Instance
	root := f.dir
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	names, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if !n.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, n.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".json" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, n.Name(), e.Name()))
			if err != nil {
				return nil, err
			}
			var in Instance
			if err := json.Unmarshal(data, &in); err != nil {
				return nil, err
			}
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].UUID < out[j].UUID
	})
	return out, nil
}
