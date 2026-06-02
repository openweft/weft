package main

// plugin_federation_test.go exercises the Plugin + Federation RPC
// surfaces added on top of weftServer. The Manager seam (pluginManager)
// is mocked directly so the tests stay hermetic — no real catalogue
// directory walk, no agent client dial, no Poller goroutine.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/federation"
	"github.com/openweft/weft/pluginstore"
)

// fakePluginManager is a stand-in for the realPluginManager backed by
// in-memory state. Each method respects the same contract as the real
// one, so the server handlers see no behavioural difference between
// the two.
type fakePluginManager struct {
	catalogue map[string]*pluginstore.Manifest
	installed []pluginstore.Instance
	installFn func(ctx context.Context, name, project string, inputs map[string]any) (pluginstore.Instance, error)
}

func (f *fakePluginManager) LoadCatalogue() (map[string]*pluginstore.Manifest, error) {
	return f.catalogue, nil
}

func (f *fakePluginManager) ListInstalled() ([]pluginstore.Instance, error) {
	return f.installed, nil
}

func (f *fakePluginManager) Install(ctx context.Context, name, project string, inputs map[string]any) (pluginstore.Instance, error) {
	if f.installFn != nil {
		return f.installFn(ctx, name, project, inputs)
	}
	if _, ok := f.catalogue[name]; !ok {
		return pluginstore.Instance{}, errors.New("not found")
	}
	inst := pluginstore.Instance{
		Name:        name,
		UUID:        pluginstore.DeterministicInstanceUUID(name, project, asStringMap(inputs)),
		Project:     project,
		InstalledAt: time.Now(),
	}
	f.installed = append(f.installed, inst)
	return inst, nil
}

func asStringMap(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// ── ListInstalledPlugins : empty store ──────────────────────────────────

func TestListInstalledPlugins_Empty(t *testing.T) {
	s := &weftServer{plugins: &fakePluginManager{}}
	resp, err := s.ListInstalledPlugins(context.Background(), &weftv1.ListInstalledPluginsRequest{})
	if err != nil {
		t.Fatalf("ListInstalledPlugins: %v", err)
	}
	if len(resp.Instances) != 0 {
		t.Fatalf("want 0 instances, got %d", len(resp.Instances))
	}
}

// ── ListInstalledPlugins : after Install, the row appears ──────────────

func TestListInstalledPlugins_AfterInstall(t *testing.T) {
	cat := map[string]*pluginstore.Manifest{
		"valkey-cache": {Name: "valkey-cache", Version: "v1", Kind: "cache"},
	}
	fm := &fakePluginManager{catalogue: cat}
	s := &weftServer{plugins: fm}

	// Install — server-side handler should return a non-empty UUID.
	installResp, err := s.InstallPlugin(context.Background(), &weftv1.InstallPluginRequest{
		Name:    "valkey-cache",
		Project: "platform",
		Inputs:  map[string]string{"memory_mb": "512"},
	})
	if err != nil {
		t.Fatalf("InstallPlugin: %v", err)
	}
	if installResp.InstanceUuid == "" {
		t.Fatalf("InstallPlugin returned empty instance_uuid")
	}

	// Re-list — the freshly installed instance must appear.
	listResp, err := s.ListInstalledPlugins(context.Background(), &weftv1.ListInstalledPluginsRequest{})
	if err != nil {
		t.Fatalf("ListInstalledPlugins: %v", err)
	}
	if len(listResp.Instances) != 1 {
		t.Fatalf("want 1 instance, got %d", len(listResp.Instances))
	}
	row := listResp.Instances[0]
	if row.Name != "valkey-cache" {
		t.Fatalf("want name=valkey-cache, got %q", row.Name)
	}
	if row.InstanceUuid != installResp.InstanceUuid {
		t.Fatalf("listed UUID %q does not match installed %q", row.InstanceUuid, installResp.InstanceUuid)
	}
	if row.Project != "platform" {
		t.Fatalf("want project=platform, got %q", row.Project)
	}
	if row.InstalledAtUnixNs == 0 {
		t.Fatalf("want non-zero installed_at_unix_ns")
	}
}

// ── InstallPlugin : unknown plugin rejected at the handler ─────────────

func TestInstallPlugin_RejectsUnknownName(t *testing.T) {
	fm := &fakePluginManager{
		catalogue: map[string]*pluginstore.Manifest{
			"valkey-cache": {Name: "valkey-cache", Version: "v1", Kind: "cache"},
		},
	}
	s := &weftServer{plugins: fm}

	_, err := s.InstallPlugin(context.Background(), &weftv1.InstallPluginRequest{
		Name:    "ghost-plugin", // not in catalogue
		Project: "platform",
	})
	if err == nil {
		t.Fatalf("expected install of unknown plugin to fail")
	}
}

// ── ListPluginCatalogue : entries are surfaced verbatim ────────────────

func TestListPluginCatalogue_SurfacesInputs(t *testing.T) {
	cat := map[string]*pluginstore.Manifest{
		"valkey-cache": {
			Name:        "valkey-cache",
			Version:     "v1",
			Kind:        "cache",
			Description: "Managed Valkey cluster.",
			Inputs: []pluginstore.Input{
				{Name: "memory_mb", Type: "int", Default: "512", Required: true, Help: "RAM in MiB"},
				{Name: "auth_password", Type: "string", Secret: true},
			},
			VMs: []pluginstore.VMSpec{{Name: "valkey", Image: "ghcr.io/openweft/valkey:v1"}},
		},
	}
	s := &weftServer{plugins: &fakePluginManager{catalogue: cat}}

	resp, err := s.ListPluginCatalogue(context.Background(), &weftv1.ListPluginCatalogueRequest{})
	if err != nil {
		t.Fatalf("ListPluginCatalogue: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(resp.Entries))
	}
	entry := resp.Entries[0]
	if entry.Name != "valkey-cache" || entry.Kind != "cache" || entry.Version != "v1" {
		t.Fatalf("entry mismatch: %+v", entry)
	}
	if len(entry.Inputs) != 2 {
		t.Fatalf("want 2 inputs, got %d", len(entry.Inputs))
	}
	// Inputs are sorted by name inside Manifest.Validate, but since
	// the fake manifest was hand-built we can't rely on that here ;
	// look up each entry by name instead.
	byName := map[string]*weftv1.PluginInput{}
	for _, in := range entry.Inputs {
		byName[in.Name] = in
	}
	mem, ok := byName["memory_mb"]
	if !ok {
		t.Fatalf("memory_mb input missing")
	}
	if mem.Type != "int" || !mem.Required || mem.Default != "512" {
		t.Fatalf("memory_mb mapping: %+v", mem)
	}
	pwd, ok := byName["auth_password"]
	if !ok {
		t.Fatalf("auth_password input missing")
	}
	if !pwd.Secret {
		t.Fatalf("auth_password expected Secret=true, got %+v", pwd)
	}
}

// ── ListFederationPeers : status classification (live / stale / unreachable) ─

func TestListFederationPeers_StatusClassification(t *testing.T) {
	// Build a Poller with three peers and drive PollOnce + clock so
	// each lands in a distinct status bucket.
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	makePubKey := func(t *testing.T) ed25519.PublicKey {
		t.Helper()
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		return pub
	}

	peers := []federation.PeerConfig{
		{Name: "live-peer", URL: "https://live.example", PublicKey: makePubKey(t)},
		{Name: "stale-peer", URL: "https://stale.example", PublicKey: makePubKey(t)},
		{Name: "unreachable-peer", URL: "https://gone.example", PublicKey: makePubKey(t)},
	}

	p := &federation.Poller{
		Peers:    peers,
		Interval: time.Minute, // never fires — we drive Snapshot manually
		StaleTTL: 5 * time.Minute,
		Now:      clock,
	}
	// Start populates the internal states map ; cancel immediately so
	// the background loop's first pollAll round doesn't race.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("poller Start: %v", err)
	}
	p.Stop()

	// Re-create the state map directly so the test doesn't depend on
	// HTTP plumbing. Inject manifest with matching public_endpoints so
	// region + weight surface on the wire.
	setPeerState(t, p, "https://live.example", &federation.PeerState{
		Name:     "live-peer",
		URL:      "https://live.example",
		LastSeen: now.Add(-30 * time.Second), // within StaleTTL
		Manifest: &federation.FederationManifest{
			Name:    "live",
			Version: 1,
			Members: []federation.Cluster{{
				Name:            "live-peer",
				Region:          "eu-west-3",
				Weight:          150,
				PublicEndpoints: []string{"https://live.example"},
			}},
		},
	})
	setPeerState(t, p, "https://stale.example", &federation.PeerState{
		Name:     "stale-peer",
		URL:      "https://stale.example",
		LastSeen: now.Add(-10 * time.Minute), // outside StaleTTL
	})
	setPeerState(t, p, "https://gone.example", &federation.PeerState{
		Name:      "unreachable-peer",
		URL:       "https://gone.example",
		LastError: "dial tcp: connection refused",
	})

	s := &weftServer{federationPoller: p}
	resp, err := s.ListFederationPeers(context.Background(), &weftv1.ListFederationPeersRequest{})
	if err != nil {
		t.Fatalf("ListFederationPeers: %v", err)
	}
	if len(resp.Peers) != 3 {
		t.Fatalf("want 3 peers, got %d", len(resp.Peers))
	}
	byName := map[string]*weftv1.FederationPeerInfo{}
	for _, p := range resp.Peers {
		byName[p.Name] = p
	}
	if got := byName["live-peer"]; got == nil || got.Status != "live" {
		t.Fatalf("live-peer status: %+v", got)
	}
	if got := byName["live-peer"]; got != nil {
		if got.Region != "eu-west-3" {
			t.Fatalf("live-peer region: %q", got.Region)
		}
		if got.Weight != 150 {
			t.Fatalf("live-peer weight: %d", got.Weight)
		}
		if got.LastSeenUnixNs == 0 {
			t.Fatalf("live-peer last_seen_unix_ns should be set")
		}
	}
	if got := byName["stale-peer"]; got == nil || got.Status != "stale" {
		t.Fatalf("stale-peer status: %+v", got)
	}
	if got := byName["unreachable-peer"]; got == nil || got.Status != "unreachable" {
		t.Fatalf("unreachable-peer status: %+v", got)
	}
}

// setPeerState swaps in a synthetic state row for a peer URL. Used by
// the federation status test to drive classification deterministically
// without HTTP plumbing. Accesses the package-private map via the
// in-tree test helper added in federation/poller_test_helpers.go.
func setPeerState(t *testing.T, p *federation.Poller, url string, st *federation.PeerState) {
	t.Helper()
	federation.SetPeerStateForTest(p, url, st)
}
