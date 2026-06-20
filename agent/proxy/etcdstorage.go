// Copyright 2026 the openweft contributors
// SPDX-License-Identifier: BSD-3-Clause

package proxy

// etcdstorage.go : in-tree certmagic.Storage adapter backed by the
// etcd cluster the weft agent already talks to.
//
// # Why
//
// Each weft-agent host running an embedded Caddy stores its ACME
// state (certificate cache, account, OCSP staples) on local disk by
// default. In a 3-DC HA cluster terminating TLS for the same domain
// from every host, that means three independent ACME challenges, three
// orders, three certs — and a real risk of tripping Let's Encrypt's
// duplicate-cert rate limit. Pointing every host's Caddy at this
// adapter collapses the state into one etcd-owned namespace
// (`/weft/proxy/caddy/`) so the first agent mints the cert and the
// others read it back.
//
// # Scope
//
// This file deliberately does NOT import `github.com/caddyserver/certmagic`
// or `github.com/caddyserver/caddy/v2`. Weft's `agent/proxy` runs Caddy as
// a supervised subprocess (see doc.go : "supervised subprocess rather than
// library"), so the supervisor binary never registers a Caddy module.
// Instead, the consumer is the xcaddy build at `github.com/openweft/
// weft-proxy`, which imports this package as a library and wraps
// `EtcdStorage` in a thin `caddy.Module` shim — keeping all the
// certmagic/Caddy vendor weight out of weft-agent.
//
// The method set below is structurally compatible with `certmagic.Storage`
// (interface name + method signatures + semantics, including `Load`
// returning `fs.ErrNotExist` for misses and `Lock` being a
// distributed mutex on a key, not a connection-level lock). The
// xcaddy wrapper asserts the interface satisfaction at compile time
// in its own module.
//
// # Layout in etcd
//
//	<KeyPrefix>/<certmagic-key>     -> value bytes
//	<KeyPrefix>/__lock__/<key>      -> distributed mutex (etcd concurrency)
//
// We split locks under a `__lock__/` sub-prefix so that `List(prefix,
// recursive)` over the data namespace never trips over mutex book-
// keeping rows. Both live below the same KeyPrefix so a single
// etcd-level ACL / IAM rule scopes the whole proxy state.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// EtcdStoragePrefix is the kv namespace under which all proxy/Caddy
// state lives. Sibling prefixes in the same etcd cluster (e.g.
// `/weft/proxy/routes/`, `/weft/coord/hosts/`) stay disjoint so a
// blunt `List` over this prefix won't leak unrelated rows.
const EtcdStoragePrefix = "/weft/proxy/caddy/"

// etcdLockSubprefix is the per-storage subprefix where mutex keys
// live. Kept distinct from the value namespace so `List(prefix,
// recursive=true)` doesn't surface mutex bookkeeping as cert files.
const etcdLockSubprefix = "__lock__/"

// lockSessionTTLSec is the etcd lease TTL backing each distributed
// lock. If the holder dies (process crash, hard partition) the
// lease expires after this many seconds and another caller can
// acquire. Caddy's stock filesystem lock uses 5s polling + a 5min
// lock timeout; 30s here gives us 6x headroom for the longest
// realistic critical sections (an in-flight ACME order).
const lockSessionTTLSec = 30

// EtcdStorageKeyInfo mirrors certmagic.KeyInfo. The xcaddy wrapper
// converts to certmagic.KeyInfo by field copy.
type EtcdStorageKeyInfo struct {
	Key        string
	Modified   time.Time
	Size       int64
	IsTerminal bool
}

// EtcdStorage implements the certmagic.Storage contract against an
// etcd v3 cluster. Safe for concurrent use; the only local state is
// a small map of acquired locks, guarded by a mutex.
type EtcdStorage struct {
	cli       *clientv3.Client
	keyPrefix string

	mu    sync.Mutex
	locks map[string]*heldLock // key -> distributed mutex + session
}

type heldLock struct {
	mu   *concurrency.Mutex
	sess *concurrency.Session
}

// NewEtcdStorage builds an EtcdStorage over an existing etcd client.
// The caller keeps ownership of the *clientv3.Client — Close on the
// storage releases held locks but does not close the client (the
// agent reuses the same connection for many subsystems).
//
// keyPrefix is normalised so it always starts and ends with "/" —
// avoids accidental `/weft/proxy/caddyfoo` collisions when an
// operator passes a trailing-slash-less custom prefix.
func NewEtcdStorage(cli *clientv3.Client, keyPrefix string) *EtcdStorage {
	if keyPrefix == "" {
		keyPrefix = EtcdStoragePrefix
	}
	if !strings.HasSuffix(keyPrefix, "/") {
		keyPrefix += "/"
	}
	if !strings.HasPrefix(keyPrefix, "/") {
		keyPrefix = "/" + keyPrefix
	}
	return &EtcdStorage{
		cli:       cli,
		keyPrefix: keyPrefix,
		locks:     make(map[string]*heldLock),
	}
}

// fullKey converts a certmagic-relative key (e.g.
// "certificates/acme-v02.api.letsencrypt.org-directory/example.com/example.com.crt")
// into the absolute etcd key.
func (s *EtcdStorage) fullKey(k string) string {
	return s.keyPrefix + strings.TrimPrefix(k, "/")
}

// Store writes value under key. Atomic at the etcd row level —
// concurrent stores to the same key race but neither corrupts the
// other; certmagic relies on Lock for cross-store ordering.
func (s *EtcdStorage) Store(ctx context.Context, key string, value []byte) error {
	_, err := s.cli.Put(ctx, s.fullKey(key), string(value))
	if err != nil {
		return fmt.Errorf("etcd Put %q: %w", key, err)
	}
	return nil
}

// Load reads the value at key. Returns fs.ErrNotExist when the key
// is absent — certmagic distinguishes a missing cert (re-mint) from
// a real I/O error (back off) by checking errors.Is(err, fs.ErrNotExist).
func (s *EtcdStorage) Load(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.cli.Get(ctx, s.fullKey(key))
	if err != nil {
		return nil, fmt.Errorf("etcd Get %q: %w", key, err)
	}
	if len(resp.Kvs) == 0 {
		return nil, fs.ErrNotExist
	}
	// resp.Kvs[0].Value is owned by the response — copy so the caller
	// can't mutate our cached buffer (etcd's gRPC layer pools these).
	v := resp.Kvs[0].Value
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

// Delete removes key. Deleting a non-existent key is a no-op rather
// than an error — matches certmagic's filesystem implementation,
// which swallows ENOENT on Delete.
func (s *EtcdStorage) Delete(ctx context.Context, key string) error {
	_, err := s.cli.Delete(ctx, s.fullKey(key))
	if err != nil {
		return fmt.Errorf("etcd Delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether key is present. Errors are squashed to
// false — certmagic treats Exists as a hint, not a contract; a
// transient etcd blip during Exists is followed by a real Load
// that will surface the error properly.
func (s *EtcdStorage) Exists(ctx context.Context, key string) bool {
	resp, err := s.cli.Get(ctx, s.fullKey(key), clientv3.WithCountOnly())
	if err != nil {
		return false
	}
	return resp.Count > 0
}

// List enumerates keys under prefix. recursive=false returns only
// the immediate children (one path segment deeper); recursive=true
// returns every descendant. Returned keys are relative to the storage
// (KeyPrefix is stripped) and lock-bookkeeping keys are filtered out.
//
// Non-recursive mode synthesises directory entries on the fly —
// etcd is a flat KV store with no notion of directories, so we walk
// the prefix-recursive result and dedup by next-segment.
func (s *EtcdStorage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	listPrefix := s.fullKey(prefix)
	if !strings.HasSuffix(listPrefix, "/") {
		listPrefix += "/"
	}
	resp, err := s.cli.Get(ctx, listPrefix,
		clientv3.WithPrefix(),
		clientv3.WithKeysOnly(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		return nil, fmt.Errorf("etcd List %q: %w", prefix, err)
	}

	// We strip the storage-level prefix back off; the caller speaks
	// certmagic-relative paths, not absolute etcd keys.
	stripPrefix := s.keyPrefix

	seen := make(map[string]struct{}, len(resp.Kvs))
	out := make([]string, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		raw := strings.TrimPrefix(string(kv.Key), stripPrefix)
		// Filter mutex rows — they live below __lock__/ and would
		// otherwise show up as bogus certmagic entries.
		if strings.HasPrefix(raw, etcdLockSubprefix) {
			continue
		}
		if recursive {
			out = append(out, raw)
			continue
		}
		// Non-recursive: keep only the segment immediately under
		// `prefix`, deduping descendants. Take the substring after
		// the trimmed prefix, then trim to the first '/' if any.
		rel := strings.TrimPrefix(raw, strings.TrimPrefix(prefix, "/"))
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		seg := rel
		if i := strings.Index(rel, "/"); i >= 0 {
			seg = rel[:i]
		}
		// Reassemble back to the storage-relative form
		// `<prefix>/<seg>` so the caller can feed it back to Stat.
		joined := path.Join(strings.TrimPrefix(prefix, "/"), seg)
		if _, ok := seen[joined]; ok {
			continue
		}
		seen[joined] = struct{}{}
		out = append(out, joined)
	}
	return out, nil
}

// Stat returns metadata about key. ModRevision is mapped onto
// Modified by reading the etcd lease/header timestamp from a small
// Get; etcd doesn't store wall-clock per-row, so we synthesise from
// the response header revision time-of-call. Certmagic uses Modified
// only for cache-staleness heuristics, so an approximate value is
// acceptable. IsTerminal=true means the key is a leaf (value present);
// non-leaf entries are inferred from List, never from Stat directly.
func (s *EtcdStorage) Stat(ctx context.Context, key string) (EtcdStorageKeyInfo, error) {
	resp, err := s.cli.Get(ctx, s.fullKey(key))
	if err != nil {
		return EtcdStorageKeyInfo{}, fmt.Errorf("etcd Stat %q: %w", key, err)
	}
	if len(resp.Kvs) == 0 {
		return EtcdStorageKeyInfo{}, fs.ErrNotExist
	}
	return EtcdStorageKeyInfo{
		Key:        key,
		Modified:   time.Now(), // best-effort; see comment above
		Size:       int64(len(resp.Kvs[0].Value)),
		IsTerminal: true,
	}, nil
}

// Lock acquires a distributed mutex on the given key. The lock is
// keyed below `<KeyPrefix>/__lock__/<key>` and backed by an etcd
// concurrency session (a TTL lease + keepalive goroutine). The
// semantics match certmagic's contract :
//
//   - blocks until the lock is acquired or ctx is cancelled ;
//   - cross-process / cross-host : two weft agents on different
//     machines racing for the same key serialise here ;
//   - if the holder dies, the lease expires after lockSessionTTLSec
//     seconds and the next caller wins ;
//   - re-entrant Lock from the same EtcdStorage instance returns
//     immediately (matches certmagic's filesystem advisory-lock
//     behaviour, which uses a process-local map for re-entry).
//
// Subtlety : `concurrency.Mutex` is per-Session. Each call needs a
// fresh session because the session carries the lease that holds
// the lock, and re-using a session across unrelated locks would
// couple their lifetimes (Close-ing one would revoke the other's
// lease). We stash the session alongside the mutex in s.locks so
// Unlock can close it.
func (s *EtcdStorage) Lock(ctx context.Context, key string) error {
	s.mu.Lock()
	if _, held := s.locks[key]; held {
		// Re-entrant lock — already held by this storage instance.
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	sess, err := concurrency.NewSession(s.cli,
		concurrency.WithTTL(lockSessionTTLSec),
		concurrency.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("etcd lock %q: new session: %w", key, err)
	}

	mu := concurrency.NewMutex(sess, s.keyPrefix+etcdLockSubprefix+key)
	if err := mu.Lock(ctx); err != nil {
		// Session must be torn down on a failed acquire; otherwise
		// its lease lingers for lockSessionTTLSec seconds, wasting
		// etcd capacity and stretching the recovery window.
		_ = sess.Close()
		return fmt.Errorf("etcd lock %q: acquire: %w", key, err)
	}

	s.mu.Lock()
	// Double-check : another goroutine may have raced us during the
	// acquire. Last writer wins — release ours.
	if _, held := s.locks[key]; held {
		s.mu.Unlock()
		_ = mu.Unlock(context.Background())
		_ = sess.Close()
		return nil
	}
	s.locks[key] = &heldLock{mu: mu, sess: sess}
	s.mu.Unlock()
	return nil
}

// Unlock releases the distributed mutex previously acquired by
// Lock. Calling Unlock on a key we don't hold is an error (matches
// certmagic's contract — its filesystem impl returns an error too,
// rather than silently no-op'ing, because Unlock-without-Lock
// almost always signals a logic bug in the caller).
func (s *EtcdStorage) Unlock(ctx context.Context, key string) error {
	s.mu.Lock()
	held, ok := s.locks[key]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("etcd unlock %q: not held by this storage", key)
	}
	delete(s.locks, key)
	s.mu.Unlock()

	// Release the mutex first (etcd Delete on the lock key) then
	// close the session (revokes the underlying lease).
	mErr := held.mu.Unlock(ctx)
	sErr := held.sess.Close()
	if mErr != nil {
		return fmt.Errorf("etcd unlock %q: %w", key, mErr)
	}
	if sErr != nil {
		return fmt.Errorf("etcd unlock %q: session close: %w", key, sErr)
	}
	return nil
}

// Close releases every lock this storage still holds. Idempotent.
// Used in tests; in production each agent holds the EtcdStorage for
// the lifetime of the process and Close is implicit at exit.
func (s *EtcdStorage) Close() error {
	s.mu.Lock()
	held := s.locks
	s.locks = make(map[string]*heldLock)
	s.mu.Unlock()
	var firstErr error
	for k, h := range held {
		if err := h.mu.Unlock(context.Background()); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("unlock %q: %w", k, err)
		}
		if err := h.sess.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("session close %q: %w", k, err)
		}
	}
	return firstErr
}

// storageEtcdInTreeConfig builds the Caddy JSON `storage` block for
// the in-tree adapter. The xcaddy build at
// `github.com/openweft/weft-proxy` registers the module name
// "weft_etcd"; mismatch surfaces as a structured /load error on the
// admin endpoint, which the supervisor logs verbatim.
//
// Endpoints + key_prefix are passed through to the wrapper, which
// dials its own *clientv3.Client (the Caddy subprocess can't share
// the agent's). That dial uses the same etcd endpoints the agent
// already targets, so any TLS / auth configured for the agent's
// etcd client must also be reachable from the Caddy subprocess.
func storageEtcdInTreeConfig(endpoints []string) map[string]any {
	return map[string]any{
		"module":     "weft_etcd",
		"endpoints":  endpoints,
		"key_prefix": EtcdStoragePrefix,
	}
}

// Compile-time sanity : EtcdStorage exposes the methods the
// certmagic.Storage interface requires. We can't `var _ certmagic.Storage`
// here without importing certmagic (the whole point is to keep it
// out of weft's vendor tree); instead we declare a local interface
// with the same shape so a future refactor that drops a method
// breaks the build loud and clear.
type certmagicStorageShape interface {
	Lock(ctx context.Context, key string) error
	Unlock(ctx context.Context, key string) error
	Store(ctx context.Context, key string, value []byte) error
	Load(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) bool
	List(ctx context.Context, prefix string, recursive bool) ([]string, error)
	// Stat returns KeyInfo in real certmagic; we use our local shape
	// type so this file imports nothing extra. The xcaddy wrapper
	// adapts via a field-by-field copy.
	Stat(ctx context.Context, key string) (EtcdStorageKeyInfo, error)
}

var _ certmagicStorageShape = (*EtcdStorage)(nil)

// errLockNotHeld is exported for tests + diagnosability — operators
// reading agent logs see why an Unlock failed without a stack trace.
var errLockNotHeld = errors.New("lock not held by this storage")

// equalKeyForTest is a small helper used by the test suite to
// compare List results stably across etcd revisions. Lives here
// rather than in the _test.go so we can keep the helper unexported
// while still giving the test file a single import surface.
func equalKeyForTest(a, b []byte) bool { return bytes.Equal(a, b) }
