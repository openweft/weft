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
