package main

// embed_etcd.go — in-process etcd via go.etcd.io/etcd/server/v3/embed.
// The `embed-etcd` storage backend boots a single-member etcd inside
// the weft process, then the regular etcd client wiring dials it
// over loopback TCP. Same code path as a 3-DC etcd cluster ; no
// external infra to install. Production HA still uses `storage-backend
// = etcd` with real endpoints — this mode exists so a 1-host lab or
// single-node operator deploy can run the "real" backend without
// standing up a separate etcd.
//
// Disk layout : <configDir>/etcd-embed/data. Ports are kernel-picked
// (loopback :0) so two operators running `weft` on the same box
// don't collide. embed.Etcd's advertise URLs must be host:port form
// (raft validation), so we can't use unix sockets even though etcd
// would otherwise accept them.

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/etcd/server/v3/embed"
)

// embedEtcdHandle is the running in-process etcd + its client URLs.
// Close() stops the server ; the data dir is preserved across
// restarts so registries survive a vzd bounce.
type embedEtcdHandle struct {
	server    *embed.Etcd
	endpoints []string
	dataDir   string
}

// startEmbedEtcd boots an embed.Etcd whose client+peer listeners
// bind to free loopback ports under <baseDir>/etcd-embed. Returns
// once Server.ReadyNotify fires (so a follow-up clientv3.New dial
// doesn't race the bootstrap).
//
// Caller owns the returned handle — call Close() at shutdown to
// release the listeners + the bbolt mmap.
func startEmbedEtcd(baseDir string) (*embedEtcdHandle, error) {
	root := filepath.Join(baseDir, "etcd-embed")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("embed-etcd mkdir %s: %w", dataDir, err)
	}

	// embed.Etcd's advertise URLs must be host:port (checkHostURLs
	// in embed/config.go). Grab two free loopback ports — kernel
	// allocation keeps the listener off any fixed port so two
	// operators running `weft` on the same box don't collide.
	clientURL, err := pickLoopbackURL()
	if err != nil {
		return nil, fmt.Errorf("embed-etcd pick client port: %w", err)
	}
	peerURL, err := pickLoopbackURL()
	if err != nil {
		return nil, fmt.Errorf("embed-etcd pick peer port: %w", err)
	}

	cfg := embed.NewConfig()
	cfg.Name = "weft-embed"
	cfg.Dir = dataDir
	cfg.ListenClientUrls = []url.URL{*clientURL}
	cfg.AdvertiseClientUrls = []url.URL{*clientURL}
	cfg.ListenPeerUrls = []url.URL{*peerURL}
	cfg.AdvertisePeerUrls = []url.URL{*peerURL}
	cfg.InitialCluster = cfg.Name + "=" + peerURL.String()
	cfg.InitialClusterToken = "weft-embed"
	// Operator log noise control : embed.Etcd defaults to a chatty
	// info-level logger. "warn" keeps surprises visible without
	// drowning the weft startup output.
	cfg.LogLevel = "warn"
	cfg.LogOutputs = []string{"stderr"}

	srv, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, fmt.Errorf("embed-etcd start: %w", err)
	}

	// Wait for the cluster to be ready before returning. 30s mirrors
	// the dial timeout downstream ; on a healthy box ready fires in
	// well under a second.
	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		srv.Close()
		return nil, fmt.Errorf("embed-etcd not ready after 30s")
	}

	return &embedEtcdHandle{
		server:    srv,
		endpoints: []string{clientURL.String()},
		dataDir:   dataDir,
	}, nil
}

// Close stops the embedded etcd. Idempotent — calling twice is a
// no-op.
func (h *embedEtcdHandle) Close() error {
	if h == nil || h.server == nil {
		return nil
	}
	h.server.Close()
	h.server = nil
	return nil
}

// pickLoopbackURL grabs a free 127.0.0.1 port the kernel allocated
// for us and returns it as an http://127.0.0.1:<port> URL. There's
// a tiny race window between Close() and embed.Etcd binding the
// same port ; acceptable for an in-process backend that's only
// dialled by the same process.
func pickLoopbackURL() (*url.URL, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		return nil, err
	}
	return url.Parse("http://" + addr)
}

// embedRoundtripTimeout bounds a single Save/Load round-trip in the
// embed-etcd smoke test. Exported to the _test file via package
// scope so the test doesn't redefine its own.
const embedRoundtripTimeout = 5 * time.Second
