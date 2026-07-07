package etcdcoord

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

// embeddedEtcd boots an embed.Etcd whose client+peer listeners bind
// to free loopback ports under t.TempDir() — sibling impl to the
// production startEmbedEtcd in cmd/weft. Tiny ; <30 lines. Lets the
// test suite exercise real etcd v3 semantics (leases, watches,
// elections) without an external dep.
func embeddedEtcd(t *testing.T) *clientv3.Client {
	t.Helper()
	root := filepath.Join(t.TempDir(), "etcd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	clientURL := pickURL(t)
	peerURL := pickURL(t)

	cfg := embed.NewConfig()
	cfg.Name = "test"
	cfg.Dir = root
	cfg.ListenClientUrls = []url.URL{*clientURL}
	cfg.AdvertiseClientUrls = []url.URL{*clientURL}
	cfg.ListenPeerUrls = []url.URL{*peerURL}
	cfg.AdvertisePeerUrls = []url.URL{*peerURL}
	cfg.InitialCluster = cfg.Name + "=" + peerURL.String()
	cfg.InitialClusterToken = "weft-test"
	cfg.LogLevel = "warn"
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
		Endpoints:   []string{clientURL.String()},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial etcd: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func pickURL(t *testing.T) *url.URL {
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

func TestHostLiveness_RegisterAndStop(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx := context.Background()
	hl, err := RegisterHostLiveness(ctx, cli, HostMetadata{
		HostUUID: "host-a", Hostname: "dc1-h1", Hypervisor: "qemu",
	}, LivenessOptions{LeaseTTLSec: 5})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Key should exist immediately.
	resp, err := cli.Get(ctx, hl.Key())
	if err != nil || resp.Count != 1 {
		t.Fatalf("get after register: count=%d err=%v", resp.Count, err)
	}
	if err := hl.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Key should be gone immediately (Stop revokes).
	resp, err = cli.Get(ctx, hl.Key())
	if err != nil {
		t.Fatalf("get after stop: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("key still present after Stop ; count=%d", resp.Count)
	}
}

func TestHostLiveness_LeaseExpiresAfterClientCrash(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx := context.Background()
	hl, err := RegisterHostLiveness(ctx, cli, HostMetadata{HostUUID: "host-b"}, LivenessOptions{LeaseTTLSec: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process crash : cancel keepalive ctx without revoking.
	hl.cancel()
	// Allow keepalive goroutine to settle.
	<-hl.done

	// After TTL+slack the key should be auto-deleted by etcd.
	time.Sleep(3 * time.Second)
	resp, err := cli.Get(ctx, hl.Key())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Count != 0 {
		t.Errorf("lease did not expire after TTL ; count=%d", resp.Count)
	}
}

// TestHostLiveness_SelfHealsAfterKeepAliveClose simulates what was
// observed live on dc1-r2-h1 2026-06-26 : etcd closes the KeepAlive
// stream (network blip / leader change / lease revoked externally)
// and the agent must re-register instead of going silently dark for
// the rest of its uptime. Pre-fix, the goroutine logged a warn and
// returned — the host stayed gone from ConnectedHostUuids until the
// next manual systemctl restart.
func TestHostLiveness_SelfHealsAfterKeepAliveClose(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx := context.Background()
	hl, err := RegisterHostLiveness(ctx, cli, HostMetadata{HostUUID: "host-heal"},
		LivenessOptions{LeaseTTLSec: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer hl.Stop(ctx)
	oldLease := hl.LeaseID()
	// Force the keep-alive stream closed by revoking the lease out
	// from under the goroutine. etcd terminates the KeepAlive channel
	// + drops the key ; the goroutine should re-register a fresh
	// lease and re-put the key under it.
	if _, err := cli.Revoke(ctx, oldLease); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Wait up to 5s for self-heal (backoff starts at 100ms).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := cli.Get(ctx, hl.Key())
		if err == nil && resp.Count == 1 && hl.LeaseID() != oldLease {
			// Self-heal observed.
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	resp, _ := cli.Get(ctx, hl.Key())
	t.Errorf("self-heal did not run ; key count=%d  lease=%d  oldLease=%d",
		resp.Count, hl.LeaseID(), oldLease)
}

func TestHostWatcher_InitialSnapshot(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pre-register one host BEFORE the watcher starts.
	hl, err := RegisterHostLiveness(ctx, cli, HostMetadata{
		HostUUID: "host-existing", Hostname: "early",
	}, LivenessOptions{LeaseTTLSec: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer hl.Stop(ctx)

	w, err := NewHostWatcher(ctx, cli, WatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-w.Events():
		if ev.Kind != HostUp || ev.HostUUID != "host-existing" {
			t.Errorf("got %+v ; want HostUp/host-existing", ev)
		}
		if ev.Metadata.Hostname != "early" {
			t.Errorf("metadata hostname = %q ; want early", ev.Metadata.Hostname)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no synthetic HostUp event for pre-existing host")
	}
}

func TestHostWatcher_FiresDownOnLeaseExpiry(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := NewHostWatcher(ctx, cli, WatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}

	hl, err := RegisterHostLiveness(ctx, cli, HostMetadata{HostUUID: "host-c"}, LivenessOptions{LeaseTTLSec: 1})
	if err != nil {
		t.Fatal(err)
	}

	// Drain the Up event.
	select {
	case ev := <-w.Events():
		if ev.Kind != HostUp || ev.HostUUID != "host-c" {
			t.Errorf("first event = %+v ; want HostUp/host-c", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Up event")
	}

	// Crash the host (cancel keepalive, no revoke). Lease expires in 1s.
	hl.cancel()
	<-hl.done

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-w.Events():
			if ev.Kind == HostDown && ev.HostUUID == "host-c" {
				return
			}
		case <-deadline:
			t.Fatal("no HostDown event within 5s of lease expiry")
		}
	}
}

func TestHostWatcher_SuppressesSelf(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hl, err := RegisterHostLiveness(ctx, cli, HostMetadata{HostUUID: "me"}, LivenessOptions{LeaseTTLSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer hl.Stop(ctx)
	w, err := NewHostWatcher(ctx, cli, WatcherOptions{IncludeSelf: "me"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-w.Events():
		t.Errorf("got event for self : %+v", ev)
	case <-time.After(300 * time.Millisecond):
		// good : self is filtered out
	}
	_ = w
}

func TestElection_FirstCampaignWinsAndOthersBlock(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := "/test/election/respawn-rule-1"
	winner, err := NewElection(ctx, cli, ElectionOptions{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer winner.Close()
	if err := winner.Campaign(ctx, "host-a"); err != nil {
		t.Fatalf("winner campaign: %v", err)
	}

	// Second campaign blocks. Use a short ctx to assert it'd wait.
	loser, err := NewElection(ctx, cli, ElectionOptions{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer loser.Close()
	cctx, ccancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer ccancel()
	if err := loser.Campaign(cctx, "host-b"); err == nil {
		t.Error("loser campaign returned nil ; want ctx-deadline-exceeded")
	}
}

func TestElection_ResignAllowsSuccession(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := "/test/election/respawn-rule-2"

	first, err := NewElection(ctx, cli, ElectionOptions{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Campaign(ctx, "host-a"); err != nil {
		t.Fatal(err)
	}

	// Resign + close first.
	if err := first.Resign(ctx); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := NewElection(ctx, cli, ElectionOptions{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	cctx, ccancel := context.WithTimeout(ctx, 2*time.Second)
	defer ccancel()
	if err := second.Campaign(cctx, "host-b"); err != nil {
		t.Fatalf("second campaign after resign: %v", err)
	}
}

func TestElection_ObserveStreamsLeaderTransitions(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := "/test/election/respawn-rule-3"

	winner, err := NewElection(ctx, cli, ElectionOptions{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer winner.Close()
	if err := winner.Campaign(ctx, "host-leader"); err != nil {
		t.Fatal(err)
	}

	observer, err := NewElection(ctx, cli, ElectionOptions{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()

	obsCh := observer.Observe(ctx)
	select {
	case identity := <-obsCh:
		if identity != "host-leader" {
			t.Errorf("Observe yielded %q ; want host-leader", identity)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no leader observed in 3s")
	}
}

func TestElectionPool_ReusesSessionAcrossElections(t *testing.T) {
	cli := embeddedEtcd(t)
	pool := NewElectionPool(cli, PoolOptions{TTLSec: 10})
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := "/test/pool/rule-1"
	e1, err := pool.Election(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	// e1.Close() is a no-op : session belongs to pool.
	if err := e1.Close(); err != nil {
		t.Errorf("e1.Close() returned %v ; want nil (no-op)", err)
	}
	if got := pool.Stats().SessionCount; got != 1 {
		t.Errorf("after e1.Close : SessionCount=%d ; want 1 (pool keeps it)", got)
	}

	// Second Election for the SAME key reuses the session.
	e2, err := pool.Election(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if e1.session != e2.session {
		t.Error("Election#2 for same key got a different session ; pool didn't reuse")
	}
	if got := pool.Stats().SessionCount; got != 1 {
		t.Errorf("after e2 : SessionCount=%d ; want 1 (still reused)", got)
	}

	// Election for a DIFFERENT key gets its own session.
	e3, err := pool.Election(ctx, "/test/pool/rule-2")
	if err != nil {
		t.Fatal(err)
	}
	if e1.session == e3.session {
		t.Error("different keys share a session ; want separate")
	}
	if got := pool.Stats().SessionCount; got != 2 {
		t.Errorf("after rule-2 : SessionCount=%d ; want 2", got)
	}
}

func TestElectionPool_CampaignAndResignWork(t *testing.T) {
	cli := embeddedEtcd(t)
	pool := NewElectionPool(cli, PoolOptions{TTLSec: 10})
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := "/test/pool/elect-1"

	e1, _ := pool.Election(ctx, key)
	if err := e1.Campaign(ctx, "host-a"); err != nil {
		t.Fatal(err)
	}

	// A non-pool Election (= separate session) can't take it.
	loser, _ := NewElection(ctx, cli, ElectionOptions{Key: key})
	defer loser.Close()
	cctx, ccancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer ccancel()
	if err := loser.Campaign(cctx, "host-b"); err == nil {
		t.Error("loser campaign returned nil ; want deadline-exceeded")
	}

	// Resign + the loser should now win.
	if err := e1.Resign(ctx); err != nil {
		t.Fatal(err)
	}
	cctx2, ccancel2 := context.WithTimeout(ctx, 3*time.Second)
	defer ccancel2()
	if err := loser.Campaign(cctx2, "host-b"); err != nil {
		t.Errorf("loser campaign after resign : %v", err)
	}
}

func TestElectionPool_CloseRevokesAllSessions(t *testing.T) {
	cli := embeddedEtcd(t)
	pool := NewElectionPool(cli, PoolOptions{TTLSec: 30})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Grant 3 sessions.
	for i := 0; i < 3; i++ {
		_, err := pool.Election(ctx, fmt.Sprintf("/test/pool/key-%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := pool.Stats().SessionCount; got != 3 {
		t.Fatalf("SessionCount=%d ; want 3", got)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if got := pool.Stats(); !got.Closed || got.SessionCount != 0 {
		t.Errorf("Close didn't clear pool : %+v", got)
	}
	// Second Close is a no-op.
	if err := pool.Close(); err != nil {
		t.Errorf("second Close : %v", err)
	}
	// Election after Close returns error.
	if _, err := pool.Election(ctx, "/test/pool/post-close"); err == nil {
		t.Error("Election after Close should error")
	}
}

func TestElectionPool_DefaultTTL(t *testing.T) {
	cli := embeddedEtcd(t)
	pool := NewElectionPool(cli, PoolOptions{}) // zero opts
	defer pool.Close()
	if got := pool.Stats().TTLSec; got != 30 {
		t.Errorf("default TTLSec=%d ; want 30", got)
	}
}
