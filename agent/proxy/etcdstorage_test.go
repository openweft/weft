// Copyright 2026 the openweft contributors
// SPDX-License-Identifier: BSD-3-Clause

package proxy

// etcdstorage_test.go exercises EtcdStorage end-to-end against an
// embed.Etcd booted inside the test process. We mirror the fixture
// pattern from etcdcoord/etcdcoord_test.go and storage_etcd_embedded_test.go
// rather than reaching for an external etcd binary.
//
// Coverage :
//   - Store / Load / Exists / Delete happy-path round-trip
//   - Load on a missing key returns fs.ErrNotExist
//   - List with prefix : both recursive and non-recursive (directory-style)
//   - Lock contention : two goroutines race, the second blocks until
//     the first Unlocks
//   - Lock blocks against ctx cancellation
//   - Mutex-bookkeeping keys never leak into List output

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

// embeddedEtcdProxy boots a single-node embed.Etcd with both
// listeners bound to random loopback ports. The fixture is tiny on
// purpose : the test harness in etcdcoord/ already proved this
// pattern's reliable across CI runners.
func embeddedEtcdProxy(t *testing.T) *clientv3.Client {
	t.Helper()
	root := filepath.Join(t.TempDir(), "etcd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cu := pickURLProxy(t)
	pu := pickURLProxy(t)

	cfg := embed.NewConfig()
	cfg.Name = "test"
	cfg.Dir = root
	cfg.ListenClientUrls = []url.URL{*cu}
	cfg.AdvertiseClientUrls = []url.URL{*cu}
	cfg.ListenPeerUrls = []url.URL{*pu}
	cfg.AdvertisePeerUrls = []url.URL{*pu}
	cfg.InitialCluster = cfg.Name + "=" + pu.String()
	cfg.InitialClusterToken = "weft-proxy-test"
	cfg.LogLevel = "error"
	cfg.LogOutputs = []string{"stderr"}

	srv, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatalf("embed etcd: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		t.Fatal("etcd not ready in 30s")
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{cu.String()},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial etcd: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func pickURLProxy(t *testing.T) *url.URL {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	u, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestEtcdStorage_StoreLoadExistsDelete(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	st := NewEtcdStorage(cli, "")
	defer st.Close()

	ctx := context.Background()
	key := "certificates/issuer/example.com/example.com.crt"
	want := []byte("PEM-BLOB-CONTENT")

	if err := st.Store(ctx, key, want); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if !st.Exists(ctx, key) {
		t.Fatalf("Exists after Store: false, want true")
	}
	got, err := st.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalKeyForTest(got, want) {
		t.Fatalf("Load mismatch : got %q want %q", got, want)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if st.Exists(ctx, key) {
		t.Fatalf("Exists after Delete: true, want false")
	}
	if _, err := st.Load(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load after Delete : want fs.ErrNotExist, got %v", err)
	}
}

func TestEtcdStorage_LoadMissingReturnsFsErrNotExist(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	st := NewEtcdStorage(cli, "")
	defer st.Close()
	if _, err := st.Load(context.Background(), "no/such/key"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist on missing key, got %v", err)
	}
}

func TestEtcdStorage_StatHappyAndMissing(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	st := NewEtcdStorage(cli, "")
	defer st.Close()

	ctx := context.Background()
	key := "ocsp/example.com"
	val := []byte("ocsp-staple-12345")
	if err := st.Store(ctx, key, val); err != nil {
		t.Fatalf("Store: %v", err)
	}
	info, err := st.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(val)) {
		t.Fatalf("Stat.Size = %d, want %d", info.Size, len(val))
	}
	if !info.IsTerminal {
		t.Fatalf("Stat.IsTerminal = false, want true for an existing key")
	}
	if _, err := st.Stat(ctx, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat on missing key: want fs.ErrNotExist, got %v", err)
	}
}

func TestEtcdStorage_ListRecursiveAndShallow(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	st := NewEtcdStorage(cli, "")
	defer st.Close()

	ctx := context.Background()
	keys := []string{
		"certificates/issuer/a.example/a.example.crt",
		"certificates/issuer/a.example/a.example.key",
		"certificates/issuer/b.example/b.example.crt",
		"ocsp/a.example",
	}
	for _, k := range keys {
		if err := st.Store(ctx, k, []byte("v")); err != nil {
			t.Fatalf("Store %s: %v", k, err)
		}
	}

	// Recursive over "certificates" should yield every cert key.
	got, err := st.List(ctx, "certificates", true)
	if err != nil {
		t.Fatalf("List recursive: %v", err)
	}
	sort.Strings(got)
	want := []string{
		"certificates/issuer/a.example/a.example.crt",
		"certificates/issuer/a.example/a.example.key",
		"certificates/issuer/b.example/b.example.crt",
	}
	if !stringSliceEq(got, want) {
		t.Fatalf("recursive list mismatch :\n got %v\nwant %v", got, want)
	}

	// Non-recursive at "certificates/issuer" should yield the two
	// immediate child *dirs* (deduped : a.example, b.example).
	got, err = st.List(ctx, "certificates/issuer", false)
	if err != nil {
		t.Fatalf("List shallow: %v", err)
	}
	sort.Strings(got)
	want = []string{
		"certificates/issuer/a.example",
		"certificates/issuer/b.example",
	}
	if !stringSliceEq(got, want) {
		t.Fatalf("shallow list mismatch :\n got %v\nwant %v", got, want)
	}
}

func TestEtcdStorage_ListFiltersLockBookkeeping(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	st := NewEtcdStorage(cli, "")
	defer st.Close()

	ctx := context.Background()
	if err := st.Store(ctx, "data/x", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Acquire a lock — that writes to <prefix>/__lock__/<key>.
	if err := st.Lock(ctx, "data/x"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer st.Unlock(ctx, "data/x")

	// Listing the root must NOT surface the __lock__/ row.
	got, err := st.List(ctx, "", true)
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	for _, k := range got {
		if k == "" || k[0] == '_' || (len(k) >= 9 && k[:9] == "__lock__/") {
			t.Fatalf("lock bookkeeping leaked into List: %q (full=%v)", k, got)
		}
	}
}

func TestEtcdStorage_LockContention(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	stA := NewEtcdStorage(cli, "")
	stB := NewEtcdStorage(cli, "")
	defer stA.Close()
	defer stB.Close()

	ctx := context.Background()
	key := "shared/cert/example.com"

	// Goroutine A grabs the lock first, holds it briefly.
	if err := stA.Lock(ctx, key); err != nil {
		t.Fatalf("A.Lock: %v", err)
	}

	// Goroutine B tries to acquire the same key — should block
	// until A unlocks. We detect "blocked" by checking the
	// progress flag stayed false for a short period, then flips
	// true after A's Unlock.
	var bAcquired atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		bCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := stB.Lock(bCtx, key); err != nil {
			t.Errorf("B.Lock: %v", err)
			return
		}
		bAcquired.Store(true)
		_ = stB.Unlock(ctx, key)
	}()

	// Sleep briefly; B should still be blocked because A holds the lock.
	time.Sleep(300 * time.Millisecond)
	if bAcquired.Load() {
		t.Fatal("B acquired lock while A still holds it — mutex not enforced")
	}

	// Release A — B should now make progress.
	if err := stA.Unlock(ctx, key); err != nil {
		t.Fatalf("A.Unlock: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("B never acquired the lock after A released it")
	}
	if !bAcquired.Load() {
		t.Fatal("B finished without acquiring the lock")
	}
}

func TestEtcdStorage_LockRespectsContextCancel(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	stA := NewEtcdStorage(cli, "")
	stB := NewEtcdStorage(cli, "")
	defer stA.Close()
	defer stB.Close()

	ctx := context.Background()
	key := "shared/cert/cancel.example.com"

	if err := stA.Lock(ctx, key); err != nil {
		t.Fatalf("A.Lock: %v", err)
	}
	defer stA.Unlock(ctx, key)

	bCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	err := stB.Lock(bCtx, key)
	if err == nil {
		t.Fatal("B.Lock returned nil despite ctx deadline; expected an error")
	}
}

func TestEtcdStorage_LockReentrant(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	st := NewEtcdStorage(cli, "")
	defer st.Close()

	ctx := context.Background()
	if err := st.Lock(ctx, "re-entrant"); err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	// Re-entrant Lock from the same storage must return nil
	// immediately, matching certmagic's filesystem behaviour.
	if err := st.Lock(ctx, "re-entrant"); err != nil {
		t.Fatalf("re-entrant Lock: %v", err)
	}
	if err := st.Unlock(ctx, "re-entrant"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestEtcdStorage_UnlockWithoutLockErrors(t *testing.T) {
	cli := embeddedEtcdProxy(t)
	st := NewEtcdStorage(cli, "")
	defer st.Close()
	if err := st.Unlock(context.Background(), "never-locked"); err == nil {
		t.Fatal("Unlock without prior Lock: want error, got nil")
	}
}

func TestEtcdStorage_PrefixIsolation(t *testing.T) {
	// A custom KeyPrefix must scope reads + writes; storage instances
	// with different prefixes against the same etcd cluster must not
	// see each other's data.
	cli := embeddedEtcdProxy(t)
	a := NewEtcdStorage(cli, "/weft/proxy/caddy-a/")
	b := NewEtcdStorage(cli, "/weft/proxy/caddy-b/")
	defer a.Close()
	defer b.Close()

	ctx := context.Background()
	if err := a.Store(ctx, "k", []byte("from-a")); err != nil {
		t.Fatal(err)
	}
	if b.Exists(ctx, "k") {
		t.Fatal("prefix isolation broken: b sees a's key")
	}
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
