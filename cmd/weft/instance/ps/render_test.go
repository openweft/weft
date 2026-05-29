package ps

import (
	"strings"
	"testing"

	introspectv1 "github.com/openweft/weft-proto/introspectv1"
	"github.com/openweft/weft/overlay"
)

func sampleProcs() []*introspectv1.Process {
	return []*introspectv1.Process{
		{Pid: 1, Ppid: 0, User: "root", State: "S", CpuPercent: 0.06, MemPercent: 0.64, VszKb: 168944, RssKb: 13056, Tty: "?", StartTimeMs: 1600000001000, Command: "/sbin/init --system"},
		{Pid: 2, Ppid: 0, User: "root", State: "S", VszKb: 0, RssKb: 0, Command: "[kthreadd]"},
	}
}

func TestRenderTable(t *testing.T) {
	var b strings.Builder
	if err := renderTable(&b, sampleProcs()); err != nil {
		t.Fatalf("renderTable: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "USER") || !strings.Contains(out, "COMMAND") {
		t.Errorf("missing header:\n%s", out)
	}
	if !strings.Contains(out, "/sbin/init --system") {
		t.Errorf("missing pid1 command:\n%s", out)
	}
	if !strings.Contains(out, "[kthreadd]") {
		t.Errorf("missing kernel thread:\n%s", out)
	}
	// Empty tty must render as "?".
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "kthreadd") && !strings.Contains(line, "?") {
			t.Errorf("kthreadd line missing tty dash: %q", line)
		}
	}
}

func TestRenderJSON(t *testing.T) {
	var b strings.Builder
	if err := renderJSON(&b, sampleProcs()); err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSON lines, got %d:\n%s", len(lines), b.String())
	}
	if !strings.Contains(lines[0], `"pid":1`) || !strings.Contains(lines[0], `"command":"/sbin/init --system"`) {
		t.Errorf("pid1 JSON wrong: %s", lines[0])
	}
}

func TestBuildClientConfig_DefaultAllowedFromTarget(t *testing.T) {
	cfg, err := buildClientConfig("10.0.0.5:51999", "10.0.0.99", "cGVlcg==pad", "host:51820", "", 25)
	// pubkey here is arbitrary; buildClientConfig does not validate it (the
	// transport does), so this should succeed and default allowed-IP to the
	// target host /32.
	if err != nil {
		t.Fatalf("buildClientConfig: %v", err)
	}
	if len(cfg.Peer.AllowedIPs) != 1 || cfg.Peer.AllowedIPs[0].String() != "10.0.0.5/32" {
		t.Errorf("default allowed-ips = %v, want [10.0.0.5/32]", cfg.Peer.AllowedIPs)
	}
	if cfg.LocalIP.String() != "10.0.0.99" {
		t.Errorf("local ip = %v", cfg.LocalIP)
	}
}

func TestCoordsToConfig(t *testing.T) {
	target, cfg, err := coordsToConfig(overlay.Coords{
		Target:        "10.9.0.3:51999",
		PrivateKey:    "QPzhHKrOaJ6BVQUMATi/d9v+RLWHrJXTZE7vm4wZuVg=",
		LocalIP:       "10.9.0.254",
		PeerPublicKey: "eOL+2qN9l2aq/KfzlQGYCexO+T3w9lTmtzSbCtVT7ys=",
		PeerEndpoint:  "192.0.2.10:51820",
		AllowedIPs:    []string{"10.9.0.0/24"},
		Keepalive:     25,
	})
	if err != nil {
		t.Fatalf("coordsToConfig: %v", err)
	}
	if target != "10.9.0.3:51999" {
		t.Errorf("target = %s", target)
	}
	if cfg.PrivateKey == "" || cfg.PrivateKeyPath != "" {
		t.Error("coords path must use the inline private key, not a path")
	}
	if cfg.LocalIP.String() != "10.9.0.254" {
		t.Errorf("local ip = %s", cfg.LocalIP)
	}
	if cfg.Peer.PublicKey != "eOL+2qN9l2aq/KfzlQGYCexO+T3w9lTmtzSbCtVT7ys=" || cfg.Peer.Endpoint != "192.0.2.10:51820" {
		t.Errorf("peer mapped wrong: %+v", cfg.Peer)
	}
	if len(cfg.Peer.AllowedIPs) != 1 || cfg.Peer.AllowedIPs[0].String() != "10.9.0.0/24" {
		t.Errorf("allowed-ips = %v", cfg.Peer.AllowedIPs)
	}
	if cfg.Peer.PersistentKeepalive != 25 {
		t.Errorf("keepalive = %d", cfg.Peer.PersistentKeepalive)
	}
}

func TestCoordsToConfig_BadLocalIP(t *testing.T) {
	if _, _, err := coordsToConfig(overlay.Coords{LocalIP: "nope", AllowedIPs: []string{"10.0.0.0/24"}}); err == nil {
		t.Error("expected error for invalid local_ip")
	}
}

func TestBuildClientConfig_RequiredFlags(t *testing.T) {
	if _, err := buildClientConfig("10.0.0.5:51999", "", "k", "e", "", 0); err == nil {
		t.Error("expected error when --wg-local-ip missing")
	}
	if _, err := buildClientConfig("10.0.0.5:51999", "10.0.0.99", "", "e", "", 0); err == nil {
		t.Error("expected error when --wg-peer-key missing")
	}
	if _, err := buildClientConfig("10.0.0.5:51999", "10.0.0.99", "k", "", "", 0); err == nil {
		t.Error("expected error when --wg-peer-endpoint missing")
	}
}
