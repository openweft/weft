package main

// storage_factory.go translates the operator-chosen storage
// backend (`storage-backend` flag or HCL config) into a closure
// the Adapter consumes via weft.SetProjectsStorage / future
// SetUsersStorage / etc. The factory is invoked once per
// registry, lazily, when the registry first wants to load.
//
// Two backends today:
//
//   * "file" (default) — per-registry HCL file under <vmsDir>/.<name>.hcl,
//     atomic tmp+rename. Single-host dev. Returns a nil factory so the
//     Adapter falls back to its built-in FileStorageInDir behaviour.
//
//   * "etcd" — 3-DC etcd cluster per [[etcd-control-plane]]. One
//     etcd v3 client is opened at factory-build time and shared
//     across every registry. Each registry calls factory(name) and
//     gets back an EtcdStorage bound to `<key-prefix>/<name>`,
//     wired onto the shared client. Closing the connection is the
//     factory's caller's responsibility — main keeps the *Closer
//     around until shutdown.
//
//   * "embed-etcd" — in-process embed.Etcd booted under
//     <configDir>/etcd-embed, dialled over loopback TCP on a
//     kernel-picked free port. Same downstream code path as "etcd"
//     (EtcdStorage on a real clientv3 connection) ; the only
//     difference is who owns the server. Intended for single-host
//     operator deploys that want the real prod backend without
//     standing up a separate cluster.

import (
	"context"
	"fmt"
	"time"

	"github.com/openweft/weft"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// storageFactory bundles the per-registry constructor + a tear-down
// hook. Close() is called at weft shutdown to release any shared
// connection (etcd) the factory keeps alive.
//
// etcdClient, when non-nil, is the shared *clientv3.Client backing
// the etcd / embed-etcd backend. It is exposed (read-only — never
// closed by the caller) so other subsystems that need to talk to
// the same etcd as the registries can piggyback on the connection
// without re-dialling. The proxy plane (bootProxy in proxy.go) is
// the first consumer ; future ones (e.g. dynamic-config watchers
// living outside the storage stack) can do the same. nil for the
// "file" backend — bootProxy treats that as "supervisor-only".
type storageFactory struct {
	new func(name string) weft.Storage
	// newKV, when non-nil, returns a per-record KVStorage for the
	// named registry — opt-in path the registries (vmRegistry today,
	// others next) take when they want surgical per-record etcd
	// semantics instead of the full-blob Put on every mutation. nil
	// for the file backend ; constructed by newEtcdStorageFactory
	// when storage.backend = etcd / embed-etcd.
	newKV      func(name string) weft.KVStorage
	close      func() error
	etcdClient *clientv3.Client
}

// buildStorageFactory builds the closure based on the resolved
// fileConfigTargets. Returns nil `new` for the file backend so the
// Adapter uses its own FileStorageInDir default.
func buildStorageFactory(t fileConfigTargets) (*storageFactory, error) {
	backend := t.storageBackend
	if backend == "" {
		backend = "file"
	}
	switch backend {
	case "file":
		// Nil factory → Adapter applies its default file-backed
		// behaviour (NewFileStorageInDir). No external resources
		// to close.
		return &storageFactory{new: nil, close: func() error { return nil }}, nil
	case "etcd":
		if len(t.etcdEndpoints) == 0 {
			return nil, fmt.Errorf("storage backend = etcd but no endpoints configured (set storage.etcd.endpoints in weft.hcl, or use embed-etcd for in-process)")
		}
		return newEtcdStorageFactory(append([]string(nil), t.etcdEndpoints...), t.etcdUsername, t.etcdPassword, t.etcdKeyPrefix, nil)
	case "embed-etcd":
		// Spin the in-process etcd under <configDir>/etcd-embed,
		// then dial it like any other etcd. configDir is the natural
		// rooting point — same place weft.hcl lives, so the embedded
		// data dir travels with the operator's config.
		embed, err := startEmbedEtcd(t.configDir)
		if err != nil {
			return nil, err
		}
		return newEtcdStorageFactory(embed.endpoints, "", "", t.etcdKeyPrefix, embed)
	default:
		return nil, fmt.Errorf("unknown storage backend %q (want file, etcd, or embed-etcd)", backend)
	}
}

// newEtcdStorageFactory is the shared tail of the "etcd" and
// "embed-etcd" cases — open one clientv3, return a per-registry
// factory + close hook. The optional embed handle is closed after
// the client so in-flight requests drain cleanly.
func newEtcdStorageFactory(endpoints []string, username, password, prefix string, embed *embedEtcdHandle) (*storageFactory, error) {
	dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:        endpoints,
		Username:         username,
		Password:         password,
		DialTimeout:      5 * time.Second,
		AutoSyncInterval: 30 * time.Second,
		Context:          dialCtx,
	})
	if err != nil {
		if embed != nil {
			_ = embed.Close()
		}
		return nil, fmt.Errorf("etcd dial %v: %w", endpoints, err)
	}
	return &storageFactory{
		new: func(name string) weft.Storage {
			return weft.NewEtcdStorageWithClient(cli, prefix, name)
		},
		newKV: func(name string) weft.KVStorage {
			// Per-record prefix lives under <root-prefix><name>/ so
			// the legacy blob key (e.g. "vms") and the per-record
			// keys (e.g. "vms/<uuid>") never collide.
			return weft.NewEtcdKVStorageWithClient(cli, prefix+name+"/")
		},
		close: func() error {
			err := cli.Close()
			if embed != nil {
				if cerr := embed.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}
			return err
		},
		etcdClient: cli,
	}, nil
}

// displayStorageBackend returns the human-readable backend name
// for the startup log line. Empty / "file" both render as "file"
// since they mean the same thing operationally.
func displayStorageBackend(b string) string {
	switch b {
	case "":
		return "file"
	case "embed-etcd":
		return "embed-etcd (in-process)"
	default:
		return b
	}
}
