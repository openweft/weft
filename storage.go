package weft

// storage.go defines the persistence backend abstraction every
// weft registry (projects, users, networks, volumes, security
// rules, shares, …) goes through. The shape is intentionally
// minimal — the registry consumer encodes/decodes the on-disk
// format itself, the Storage just carries the blob.
//
// Why a single-blob shape:
//
//   * File backend (dev, single-host): one HCL file per registry,
//     atomic tmp+rename. Load returns the file contents; Save
//     writes a temp file and renames it into place. The HCL doc
//     stays human-readable for operators editing by hand.
//   * etcd backend (production, 3-DC cluster — see
//     etcd_control_plane.md): one etcd key per registry, the value
//     is the same HCL blob the file backend would have written.
//     Put is naturally linearizable so concurrent weft processes
//     across DCs see the same snapshot.
//   * In-memory backend (tests): a sync.Mutex + []byte; no I/O,
//     no temp files, no leftover state between test runs.
//
// All three implementations satisfy the same Storage interface, so
// the registry code (projects.go, the upcoming users.go,
// networks.go, …) doesn't care which one is wired underneath.
//
// Future direction: when a registry grows beyond what's
// comfortable to ship as a single blob (e.g. tens of thousands of
// VM records), a per-record KVStorage interface can be introduced
// alongside this one and the file/etcd impls split into two
// adapters. For projects/users/networks at the scale a single
// org typically operates, the blob shape is plenty.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Storage is the persistence backend for a single weft registry.
//
// Load returns the registry's current blob. A registry that
// doesn't exist yet (fresh install) returns (nil, nil) — NOT an
// error — so the consumer can start from an empty doc.
//
// Save atomically replaces the registry's blob. Partial writes
// MUST NOT be visible to other readers; implementations either
// use tmp+rename (FileStorage) or a single Put (etcd).
type Storage interface {
	Load(ctx context.Context) ([]byte, error)
	Save(ctx context.Context, blob []byte) error
}

// ----------------------------------------------------------------
// FileStorage — atomic-rename on-disk backend.
// ----------------------------------------------------------------

// FileStorage persists a registry as a single HCL file on local
// disk. Save writes to <path>.tmp first and renames it over the
// target so a reader never sees a partial blob.
type FileStorage struct {
	path string
}

// NewFileStorage returns a FileStorage that reads from and writes
// to the given absolute path. The path's parent directory must
// already exist; FileStorage does not call os.MkdirAll.
func NewFileStorage(path string) *FileStorage {
	return &FileStorage{path: path}
}

// Load reads the registry blob. Returns (nil, nil) when the file
// is absent (fresh install). Any other I/O error is propagated.
func (s *FileStorage) Load(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("file storage load %s: %w", s.path, err)
	}
	return b, nil
}

// Save writes the blob atomically via tmp+rename.
func (s *FileStorage) Save(ctx context.Context, blob []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("file storage write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("file storage commit %s: %w", s.path, err)
	}
	return nil
}

// Path returns the on-disk location of the registry file. Useful
// for diagnostics and tests; the registry consumer doesn't need
// it for normal Load/Save.
func (s *FileStorage) Path() string { return s.path }

// PathInDir returns the conventional registry file path for a
// resource kind under vmsDir. Used by NewFileStorageInDir below
// and by callers that want to pre-create the parent.
func PathInDir(vmsDir, name string) string {
	return filepath.Join(vmsDir, "."+name+".hcl")
}

// NewFileStorageInDir returns a FileStorage for the conventional
// registry path under vmsDir: <vmsDir>/.<name>.hcl . The leading
// dot keeps the file out of ListLocal's normal walk.
func NewFileStorageInDir(vmsDir, name string) *FileStorage {
	return NewFileStorage(PathInDir(vmsDir, name))
}

// ----------------------------------------------------------------
// MemStorage — in-memory backend for tests.
// ----------------------------------------------------------------

// MemStorage holds the registry blob in process memory. Safe for
// concurrent Load/Save via an internal mutex.
type MemStorage struct {
	mu   sync.Mutex
	blob []byte // nil until first Save; mimics "registry doesn't exist yet"
}

// NewMemStorage returns an empty in-memory Storage.
func NewMemStorage() *MemStorage { return &MemStorage{} }

// NewMemStorageWith returns an in-memory Storage pre-loaded with
// `seed`. The slice is copied so callers can reuse the buffer.
func NewMemStorageWith(seed []byte) *MemStorage {
	cp := append([]byte(nil), seed...)
	return &MemStorage{blob: cp}
}

func (s *MemStorage) Load(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blob == nil {
		return nil, nil
	}
	return append([]byte(nil), s.blob...), nil
}

func (s *MemStorage) Save(ctx context.Context, blob []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blob = append([]byte(nil), blob...)
	return nil
}

// ----------------------------------------------------------------
// EtcdStorage — stub for the etcd-backed production path.
// ----------------------------------------------------------------

// EtcdConfig captures the connection parameters for an
// etcd-backed Storage. Endpoints across at least 3 DCs per the
// control-plane design (see etcd_control_plane.md).
type EtcdConfig struct {
	// Endpoints is the list of etcd peer URLs, e.g.
	// "https://etcd-dc1.example.com:2379". The client picks one
	// transparently and follows the cluster across leader
	// changes.
	Endpoints []string
	// Username / Password authenticate the weft identity to etcd.
	// Empty means anonymous (dev only).
	Username string
	Password string
	// KeyPrefix scopes every registry key under a tenant /
	// cluster prefix (e.g. "/weft/prod/"). The full key for a
	// registry named "projects" is `KeyPrefix + "projects"`.
	KeyPrefix string
}

// EtcdStorage is the etcd-backed implementation of Storage
// (production path per etcd_control_plane.md). One EtcdStorage
// per registry; each maps to a single etcd key holding the HCL
// blob the registry would have written to disk under FileStorage.
//
// Concurrency: etcd Put is naturally linearizable, so two weft
// processes (different DCs) writing to the same registry land in
// a strict order. Blind last-writer-wins is acceptable for the
// registries we have today (projects, users, …); a future
// per-registry CAS path can be added when we identify one where
// it isn't.
type EtcdStorage struct {
	client *clientv3.Client
	key    string
	owned  bool // close client on Close() when we created it
}

// NewEtcdStorage opens an etcd client against cfg.Endpoints and
// returns a Storage bound to `cfg.KeyPrefix + name`. The caller
// owns the EtcdStorage and must Close() it — that also closes the
// underlying etcd client connection.
//
// For multiple registries sharing one etcd cluster, prefer
// NewEtcdStorageWithClient so the underlying client connection
// is shared.
func NewEtcdStorage(ctx context.Context, cfg EtcdConfig, name string) (*EtcdStorage, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("etcd storage: no endpoints configured")
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:        cfg.Endpoints,
		Username:         cfg.Username,
		Password:         cfg.Password,
		DialTimeout:      5 * time.Second,
		AutoSyncInterval: 30 * time.Second,
		Context:          ctx,
	})
	if err != nil {
		return nil, fmt.Errorf("etcd dial %v: %w", cfg.Endpoints, err)
	}
	return &EtcdStorage{
		client: cli,
		key:    cfg.KeyPrefix + name,
		owned:  true,
	}, nil
}

// NewEtcdStorageWithClient wires a Storage onto an already-open
// etcd client. The Storage does NOT take ownership: Close() leaves
// the client alive so other registries sharing it keep working.
func NewEtcdStorageWithClient(cli *clientv3.Client, keyPrefix, name string) *EtcdStorage {
	return &EtcdStorage{
		client: cli,
		key:    keyPrefix + name,
		owned:  false,
	}
}

// Load fetches the registry blob at s.key. Missing key returns
// (nil, nil) — the registry consumer treats that as "fresh
// install", same as FileStorage does for an absent file.
func (s *EtcdStorage) Load(ctx context.Context) ([]byte, error) {
	resp, err := s.client.Get(ctx, s.key)
	if err != nil {
		return nil, fmt.Errorf("etcd get %s: %w", s.key, err)
	}
	if resp.Count == 0 {
		return nil, nil
	}
	return resp.Kvs[0].Value, nil
}

// Save writes the blob to s.key. etcd's Put is linearizable across
// the 3-DC cluster, so this is the atomic-replace semantics the
// Storage contract requires.
func (s *EtcdStorage) Save(ctx context.Context, blob []byte) error {
	if _, err := s.client.Put(ctx, s.key, string(blob)); err != nil {
		return fmt.Errorf("etcd put %s: %w", s.key, err)
	}
	return nil
}

// Watch returns a channel that fires on every remote PUT/DELETE of
// s.key — the multi-host hook the registries need so writes made
// from other DCs (e.g. RegisterMicroVM's host_uuid claim on dc2)
// invalidate the local in-memory index on dc1+dc3 promptly.
//
// The channel is closed when ctx is cancelled. The blob payload is
// not delivered on the channel — receivers call Load() to fetch
// the fresh snapshot. This keeps the Storage contract honest (one
// Load = one consistent read) and avoids holding the etcd event in
// the receiver's heap during a slow reload.
//
// Implementations that can't observe remote writes (FileStorage,
// MemStorage) don't expose a Watch() method ; consumers do a type
// assertion on the WatchableStorage interface to opt in.
func (s *EtcdStorage) Watch(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 4)
	go func() {
		defer close(out)
		ch := s.client.Watch(ctx, s.key)
		for wr := range ch {
			if wr.Err() != nil {
				continue
			}
			if len(wr.Events) == 0 {
				continue
			}
			select {
			case out <- struct{}{}:
			default:
				// already a pending notification ; coalesce.
			}
		}
	}()
	return out
}

// WatchableStorage is the optional interface a Storage may implement
// to deliver "remote write happened" notifications. EtcdStorage does ;
// FileStorage and MemStorage don't (no cross-process semantics).
// Consumers detect via type assertion :
//
//	if ws, ok := storage.(WatchableStorage); ok { go reload(ws.Watch(ctx)) }
type WatchableStorage interface {
	Watch(ctx context.Context) <-chan struct{}
}

// Close releases the underlying etcd client when the Storage owns
// it (i.e. created by NewEtcdStorage). Storage created via
// NewEtcdStorageWithClient is a no-op — the caller owns the
// client.
func (s *EtcdStorage) Close() error {
	if !s.owned || s.client == nil {
		return nil
	}
	return s.client.Close()
}

// Key returns the full etcd key this Storage targets. Useful for
// diagnostics and integration tests.
func (s *EtcdStorage) Key() string { return s.key }

// ErrEtcdNotWired is retained as an exported sentinel because some
// callers may still reference it; the runtime path no longer
// returns it now that the client integration is in place.
var ErrEtcdNotWired = errors.New("etcd backend was previously unwired — this build links the etcd v3 client")
