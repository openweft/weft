package pluginstore

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

// minimalHCL is the smallest plugin manifest ParseManifest accepts
// (mirrors manifest_test.go's minimalManifest — at least one network,
// security_group, and vm block is mandatory).
const minimalHCL = `
plugin "demo" {
  version     = "v1"
  kind        = "test"
  description = "round-trip fixture"
  layout      = "ha-3dc"

  network "n1" { cidr = "10.0.0.0/24" }

  security_group "sg" {
    description = "egress"
    networks    = ["n1"]
    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
    }
  }

  vm "runner" {
    image    = "ghcr.io/example/demo:v0.1.0"
    replicas = 1
    cpu      = 1
    mem_mb   = 256
    disk_gb  = 1
    network  = "n1"
  }
}
`

// embeddedEtcd boots a single-node etcd inside the test binary so we
// don't need an external server. Mirrors etcdcoord/etcdcoord_test.go.
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
	cfg.InitialClusterToken = "weft-cat-test"
	cfg.LogLevel = "warn"
	cfg.LogOutputs = []string{"stderr"}
	srv, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(10 * time.Second):
		t.Fatal("etcd not ready")
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{clientURL.String()}, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// pickURL grabs an OS-assigned ephemeral port — the simplest race-free
// way to share the test binary across parallel embedded-etcd runs.
func pickURL(t *testing.T) *url.URL {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	u, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestEtcdCatalogue_RoundTrip(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx := context.Background()

	// Initially empty.
	cat, err := LoadCatalogueFromEtcd(ctx, cli, "")
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(cat) != 0 {
		t.Errorf("empty catalogue = %d entries ; want 0", len(cat))
	}

	// Write one, read it back.
	if err := WriteManifestsToEtcd(ctx, cli, "", map[string][]byte{"demo": []byte(minimalHCL)}); err != nil {
		t.Fatalf("write: %v", err)
	}
	cat, err = LoadCatalogueFromEtcd(ctx, cli, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cat) != 1 {
		t.Fatalf("after write : %d entries ; want 1", len(cat))
	}
	if got := cat["demo"]; got == nil || got.Name != "demo" {
		t.Errorf("missing demo manifest in %+v", cat)
	}

	// Delete : key disappears from a subsequent load.
	if err := DeleteManifestFromEtcd(ctx, cli, "", "demo"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	cat, err = LoadCatalogueFromEtcd(ctx, cli, "")
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if len(cat) != 0 {
		t.Errorf("after delete : %d entries ; want 0", len(cat))
	}
}

func TestEtcdCatalogue_RejectsPathSeparator(t *testing.T) {
	cli := embeddedEtcd(t)
	ctx := context.Background()
	err := WriteManifestsToEtcd(ctx, cli, "", map[string][]byte{"x/y": []byte(minimalHCL)})
	if err == nil {
		t.Fatal("expected error on path-separator in name")
	}
}

func TestEtcdCatalogue_NilClient(t *testing.T) {
	if _, err := LoadCatalogueFromEtcd(context.Background(), nil, ""); err == nil {
		t.Error("LoadCatalogueFromEtcd nil client : expected error")
	}
	if err := WriteManifestsToEtcd(context.Background(), nil, "", nil); err == nil {
		t.Error("WriteManifestsToEtcd nil client : expected error")
	}
	if err := DeleteManifestFromEtcd(context.Background(), nil, "", "demo"); err == nil {
		t.Error("DeleteManifestFromEtcd nil client : expected error")
	}
}
