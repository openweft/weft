package infra

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	weft "github.com/openweft/weft"
)

// -- loader: convenience accessors ------------------------------------------

// TestPlan_AccessorsAfterDefaults covers CPU/MemoryMiB/CmdlineForGuest/
// VMName/OCIImageSafe/DefaultRootfsPath/DefaultArtefact which were
// previously unused by the test sweep.
func TestPlan_AccessorsAfterDefaults(t *testing.T) {
	p := &Plan{
		Service:  "etcd",
		OCIImage: "ghcr.io/etcd-io/etcd:v3.6.0",
	}
	if err := p.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if got := p.CPU(); got != 1 {
		t.Errorf("CPU() = %d, want default 1", got)
	}
	if got := p.MemoryMiB(); got != 1024 {
		t.Errorf("MemoryMiB() = %d, want default 1024", got)
	}
	if got := p.CmdlineForGuest(); !strings.Contains(got, "rootfs0") {
		t.Errorf("CmdlineForGuest default = %q", got)
	}
	if got := p.VMName(); got != "infra-etcd" {
		t.Errorf("VMName = %q, want infra-etcd", got)
	}
	if got := p.OCIImageSafe(); got != "ghcr.io_etcd-io_etcd_v3.6.0" {
		t.Errorf("OCIImageSafe = %q", got)
	}
	if got := p.DefaultRootfsPath(); !strings.HasSuffix(got, "/rootfs") {
		t.Errorf("DefaultRootfsPath = %q (expected suffix /rootfs)", got)
	}
}

// TestCmdlineForGuest_Override : when the plan declares a cmdline, that
// string is returned verbatim (no defaulting).
func TestCmdlineForGuest_Override(t *testing.T) {
	p := &Plan{Cmdline: "custom kernel cmdline here"}
	if got := p.CmdlineForGuest(); got != "custom kernel cmdline here" {
		t.Errorf("CmdlineForGuest = %q, want override", got)
	}
}

// TestDefaultRootfsPath_XDGOverride exercises the $XDG_DATA_HOME branch
// of nclDataHome.
func TestDefaultRootfsPath_XDGOverride(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/var/lib/share")
	p := &Plan{OCIImage: "x/y:z"}
	got := p.DefaultRootfsPath()
	if !strings.Contains(got, "/var/lib/share/ncl/images/") {
		t.Errorf("DefaultRootfsPath = %q, expected XDG path", got)
	}
}

// TestDefaultArtefact pins the artefact path resolution.
func TestDefaultArtefact_Resolves(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/srv/xdg")
	got := DefaultArtefact("kernel")
	if got != filepath.Join("/srv/xdg", "ncl", "kernel") {
		t.Errorf("DefaultArtefact = %q", got)
	}
}

// TestListServices_MissingRoot surfaces an error when the infra root
// doesn't exist.
func TestListServices_MissingRoot(t *testing.T) {
	if _, err := ListServices(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error for missing root")
	}
}

// TestLoadPlan_EmptyPath surfaces a clean error.
func TestLoadPlan_EmptyPath(t *testing.T) {
	if _, err := LoadPlan(""); err == nil {
		t.Fatal("expected error for empty plan path")
	}
}

// TestLoadPlan_StatError surfaces an error for a non-existent file.
func TestLoadPlan_StatError(t *testing.T) {
	if _, err := LoadPlan(filepath.Join(t.TempDir(), "no-such.hcl")); err == nil {
		t.Fatal("expected error for nonexistent plan file")
	}
}

// TestLoadPlan_BadHCL surfaces a parse error.
func TestLoadPlan_BadHCL(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "bad.hcl")
	if err := os.WriteFile(planFile, []byte("this is not valid HCL ( ( ( "), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlan(planFile); err == nil {
		t.Fatal("expected HCL parse error")
	}
}

// TestLoadPlan_NoServiceBlock errors when the file has zero service
// blocks.
func TestLoadPlan_NoServiceBlock(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "empty.hcl")
	if err := os.WriteFile(planFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPlan(planFile)
	if err == nil {
		t.Fatal("expected error for empty plan file")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("expected 'exactly one' message, got: %v", err)
	}
}

// TestLoadPlan_MultipleServiceBlocks errors when the file has more than
// one service block.
func TestLoadPlan_MultipleServiceBlocks(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "multi.hcl")
	body := `service "a" { oci_image = "x" }
service "b" { oci_image = "y" }`
	if err := os.WriteFile(planFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPlan(planFile)
	if err == nil {
		t.Fatal("expected error for multiple service blocks")
	}
}

// TestApplyDefaults_MissingOCIImage errors when oci_image is empty.
func TestApplyDefaults_MissingOCIImage(t *testing.T) {
	p := &Plan{Service: "foo"}
	err := p.applyDefaults()
	if err == nil {
		t.Fatal("expected error for missing oci_image")
	}
	if !strings.Contains(err.Error(), "oci_image") {
		t.Errorf("error should mention oci_image, got: %v", err)
	}
}

// TestApplyDefaults_MissingLabel errors when the service block has no
// label (Service == "").
func TestApplyDefaults_MissingLabel(t *testing.T) {
	p := &Plan{OCIImage: "x"}
	if err := p.applyDefaults(); err == nil {
		t.Fatal("expected error for empty service label")
	}
}

// TestApplyDefaults_RespectsExplicitResources confirms operator-supplied
// resource values aren't overwritten by the defaults.
func TestApplyDefaults_RespectsExplicitResources(t *testing.T) {
	p := &Plan{
		Service:  "s",
		OCIImage: "x",
		Resources: &ResourcesBlk{
			CPUCount:  4,
			MemoryMiB: 8192,
		},
	}
	if err := p.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if p.CPU() != 4 || p.MemoryMiB() != 8192 {
		t.Errorf("explicit resources clobbered: cpu=%d mem=%d", p.CPU(), p.MemoryMiB())
	}
	if p.Project != "infra" {
		t.Errorf("default project = %q", p.Project)
	}
}

// TestApplyDefaults_KeepsCustomProject confirms operator-supplied
// Project field isn't overwritten.
func TestApplyDefaults_KeepsCustomProject(t *testing.T) {
	p := &Plan{Service: "s", OCIImage: "x", Project: "tenant-a"}
	if err := p.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if p.Project != "tenant-a" {
		t.Errorf("Project overwritten: %q", p.Project)
	}
}

// TestLoadAllPlans_HappyPath confirms LoadAllPlans returns each plan
// keyed by service name.
func TestLoadAllPlans_HappyPath(t *testing.T) {
	root := t.TempDir()
	for _, svc := range []string{"a", "b"} {
		dir := filepath.Join(root, "infra", svc)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `service "` + svc + `" { oci_image = "x" }`
		if err := os.WriteFile(filepath.Join(dir, "plan.hcl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := LoadAllPlans(root, []string{"a", "b"})
	if err != nil {
		t.Fatalf("LoadAllPlans: %v", err)
	}
	if len(plans) != 2 {
		t.Errorf("got %d plans, want 2", len(plans))
	}
	if plans["a"].Service != "a" || plans["b"].Service != "b" {
		t.Errorf("plans not keyed correctly: %+v", plans)
	}
}

// TestLoadAllPlans_LoadError surfaces a LoadPlan failure (missing
// plan file) up the stack.
func TestLoadAllPlans_LoadError(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadAllPlans(root, []string{"missing"}); err == nil {
		t.Fatal("expected error when plan file missing")
	}
}

// -- scheduler_bridge -------------------------------------------------------

// TestPlacementRule_NilReturnsZeroValue: a nil PlacementBlk yields an
// all-ProximityAny rule.
func TestPlacementRule_NilReturnsZeroValue(t *testing.T) {
	var b *PlacementBlk
	rule, err := b.PlacementRule()
	if err != nil {
		t.Fatalf("PlacementRule(nil): %v", err)
	}
	if rule.AZ != weft.ProximityAny || rule.Rack != weft.ProximityAny || rule.Host != weft.ProximityAny {
		t.Errorf("expected all-Any rule, got %+v", rule)
	}
}

// TestPlacementRule_AllCombinations exercises every proximity value.
func TestPlacementRule_AllCombinations(t *testing.T) {
	b := &PlacementBlk{AZ: "same", Rack: "different", Host: ""}
	rule, err := b.PlacementRule()
	if err != nil {
		t.Fatalf("PlacementRule: %v", err)
	}
	if rule.AZ != weft.ProximitySame {
		t.Errorf("AZ = %q, want same", rule.AZ)
	}
	if rule.Rack != weft.ProximityDifferent {
		t.Errorf("Rack = %q, want different", rule.Rack)
	}
	if rule.Host != weft.ProximityAny {
		t.Errorf("Host = %q, want any", rule.Host)
	}
}

// TestPlacementRule_InvalidAZ surfaces an error when the AZ field
// holds a bad string (programmer error after Validate).
func TestPlacementRule_InvalidAZ(t *testing.T) {
	b := &PlacementBlk{AZ: "wat"}
	_, err := b.PlacementRule()
	if err == nil {
		t.Fatal("expected error for unknown AZ proximity")
	}
	if !strings.Contains(err.Error(), "az") {
		t.Errorf("error should mention az, got: %v", err)
	}
}

// TestPlacementRule_InvalidRack surfaces an error from the rack arm.
func TestPlacementRule_InvalidRack(t *testing.T) {
	b := &PlacementBlk{Rack: "wat"}
	_, err := b.PlacementRule()
	if err == nil {
		t.Fatal("expected error for unknown rack proximity")
	}
	if !strings.Contains(err.Error(), "rack") {
		t.Errorf("error should mention rack, got: %v", err)
	}
}

// TestPlacementRule_InvalidHost surfaces an error from the host arm.
func TestPlacementRule_InvalidHost(t *testing.T) {
	b := &PlacementBlk{Host: "wat"}
	_, err := b.PlacementRule()
	if err == nil {
		t.Fatal("expected error for unknown host proximity")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error should mention host, got: %v", err)
	}
}

// TestGroupScheduleRequest_InvalidPlacement surfaces the placement
// rule error up the stack.
func TestGroupScheduleRequest_InvalidPlacement(t *testing.T) {
	p := &Plan{Service: "x", Placement: &PlacementBlk{AZ: "wat"}}
	_, err := p.GroupScheduleRequest("u", "h")
	if err == nil {
		t.Fatal("expected error from invalid placement")
	}
}

// -- configfile error paths -------------------------------------------------

// TestMaterialiseConfigFile_MkdirError exercises the MkdirAll failure
// branch: scratchRoot is a regular file.
func TestMaterialiseConfigFile_MkdirError(t *testing.T) {
	tmp := t.TempDir()
	asFile := filepath.Join(tmp, "scratch-but-file")
	if err := os.WriteFile(asFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Plan{
		Service: "svc",
		ConfigFile: &ConfigFileBlk{
			Path:     "/etc/svc.conf",
			Template: "ok",
		},
	}
	_, err := MaterialiseConfigFile(p, asFile, SingleReplicaContext(p))
	if err == nil {
		t.Fatal("expected mkdir failure when scratchRoot is a regular file")
	}
}

// TestBuildReplicaContextWithHost_NegativeReplicaClamps confirms the
// "replica < 1" fallback to 1.
func TestBuildReplicaContextWithHost_NegativeReplicaClamps(t *testing.T) {
	p := &Plan{
		Network:   &NetworkBlk{StaticIP: []string{"10.0.0.1"}},
		Placement: &PlacementBlk{Count: 1},
	}
	ctx := BuildReplicaContextWithHost(p, -5, "")
	if ctx.Replica != 1 {
		t.Errorf("Replica = %d, want clamp to 1", ctx.Replica)
	}
	if ctx.DC != "dc1" {
		t.Errorf("DC = %q, want dc1", ctx.DC)
	}
}

// TestBuildReplicaContext_NilPlan exercises the nil-plan branch where
// the substitution context defaults to dc1.
func TestBuildReplicaContext_NilPlan(t *testing.T) {
	ctx := BuildReplicaContext(nil, 1)
	if ctx.DC != "dc1" {
		t.Errorf("DC = %q, want dc1", ctx.DC)
	}
}

// TestBuildReplicaContext_NoNetwork: a plan with no network block
// produces a context with empty PrivateIP/Peers.
func TestBuildReplicaContext_NoNetwork(t *testing.T) {
	p := &Plan{Service: "s"}
	ctx := BuildReplicaContext(p, 1)
	if ctx.PrivateIP != "" || len(ctx.Peers) != 0 {
		t.Errorf("expected empty net fields, got %+v", ctx)
	}
}

// TestBuildReplicaContext_ReplicaOutOfBounds: static_ip array shorter
// than replica index → PrivateIP stays empty.
func TestBuildReplicaContext_ReplicaOutOfBounds(t *testing.T) {
	p := &Plan{
		Network:   &NetworkBlk{StaticIP: []string{"a", "b"}},
		Placement: &PlacementBlk{Count: 3},
	}
	ctx := BuildReplicaContext(p, 3)
	if ctx.PrivateIP != "" {
		t.Errorf("PrivateIP = %q, want empty when index out of bounds", ctx.PrivateIP)
	}
}

// -- health: error paths ----------------------------------------------------

// TestWaitHealthy_EmptyURL surfaces an error.
func TestWaitHealthy_EmptyURL(t *testing.T) {
	if err := WaitHealthy(context.Background(), "", time.Second, 100*time.Millisecond); err == nil {
		t.Fatal("expected error for empty URL")
	}
}

// TestWaitHealthy_DefaultsApplied: zero timeout / period collapse to
// the in-function defaults so the loop never blocks on bogus inputs.
func TestWaitHealthy_DefaultsApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// 0/0 → default 60s/5s — the OK response on first probe makes the loop
	// return immediately regardless.
	if err := WaitHealthy(context.Background(), srv.URL, 0, 0); err != nil {
		t.Errorf("WaitHealthy with defaults: %v", err)
	}
}

// TestWaitHealthy_ContextPreCancelled surfaces the context-cancel
// branch at the top of the loop (before any probe).
func TestWaitHealthy_ContextPreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitHealthy(ctx, "http://127.0.0.1:1", 5*time.Second, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error from pre-cancelled context")
	}
}

// TestProbeOnce_InvalidURL exercises the NewRequest error branch.
func TestProbeOnce_InvalidURL(t *testing.T) {
	client := &http.Client{Timeout: time.Second}
	ok, err := probeOnce(context.Background(), client, "http://\x7f-invalid-url")
	if ok {
		t.Error("expected ok=false")
	}
	if err == nil {
		t.Error("expected NewRequest error")
	}
}

// TestProbeOnce_BadConnection surfaces a transport error (connect refused).
func TestProbeOnce_BadConnection(t *testing.T) {
	client := &http.Client{Timeout: 50 * time.Millisecond}
	// 127.0.0.1:1 reserved — connect refused.
	ok, err := probeOnce(context.Background(), client, "http://127.0.0.1:1/")
	if ok {
		t.Error("expected ok=false")
	}
	if err == nil {
		t.Error("expected connection error")
	}
}

// TestProbeOnce_OK happy path: 200 OK from server.
func TestProbeOnce_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := &http.Client{Timeout: time.Second}
	ok, err := probeOnce(context.Background(), client, srv.URL)
	if !ok {
		t.Errorf("expected ok=true, got err=%v", err)
	}
}

// -- TopologicalSort: covers visit "done" early return ----------------------

// TestTopologicalSort_VisitedShortCircuit pins the visit-already-done
// arm of the cycle detector : a plan with multiple paths to the same
// dependency visits the dep just once.
func TestTopologicalSort_VisitedShortCircuit(t *testing.T) {
	plans := map[string]*Plan{
		"a": {Service: "a", DependsOn: []string{"b", "c"}},
		"b": {Service: "b", DependsOn: []string{"d"}},
		"c": {Service: "c", DependsOn: []string{"d"}},
		"d": {Service: "d"},
	}
	out, err := TopologicalSort(plans)
	if err != nil {
		t.Fatalf("TopologicalSort: %v", err)
	}
	// d must come before b and c; b/c before a.
	pos := map[string]int{}
	for i, p := range out {
		pos[p.Service] = i
	}
	if !(pos["d"] < pos["b"] && pos["d"] < pos["c"] && pos["b"] < pos["a"] && pos["c"] < pos["a"]) {
		t.Errorf("order broken: %+v", pos)
	}
}

// -- RenderTemplate: cover each token branch -------------------------------

// TestRenderTemplate_AllTokens exercises every supported token in a
// single template + the default fallback.
func TestRenderTemplate_AllTokens(t *testing.T) {
	ctx := TemplateContext{
		Replica:   2,
		DC:        "dc-A",
		PrivateIP: "10.1.1.2",
		Peers:     []string{"10.1.1.1", "10.1.1.3"},
		PeerDC:    "dc-B",
	}
	got := RenderTemplate("R=$REPLICA DC=$DC IP=$PRIVATE_IP P=$PEERS PDC=$PEER_DC X=$UNKNOWN", ctx)
	want := "R=2 DC=dc-A IP=10.1.1.2 P=10.1.1.1,10.1.1.3 PDC=dc-B X=$UNKNOWN"
	if got != want {
		t.Errorf("RenderTemplate:\n got %q\nwant %q", got, want)
	}
}

// -- check explicitly that toProximity returns error on unknown -------------

// TestToProximity_KnownAndUnknown exhaustively walks the helper.
func TestToProximity_KnownAndUnknown(t *testing.T) {
	cases := []struct {
		in   string
		want weft.Proximity
		err  bool
	}{
		{"", weft.ProximityAny, false},
		{"same", weft.ProximitySame, false},
		{"different", weft.ProximityDifferent, false},
		{"nope", "", true},
	}
	for _, c := range cases {
		got, err := toProximity(c.in)
		if c.err {
			if err == nil {
				t.Errorf("toProximity(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("toProximity(%q) err: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("toProximity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// -- BuildReplicaContextWithHost: zero plan or zero replicas -----------------

// TestBuildReplicaContextWithHost_NilNetwork: nil network → context
// returns early with synthetic DC.
func TestBuildReplicaContextWithHost_NilNetwork(t *testing.T) {
	p := &Plan{Service: "svc"}
	ctx := BuildReplicaContextWithHost(p, 2, "")
	if ctx.DC != "dc2" {
		t.Errorf("DC = %q, want dc2", ctx.DC)
	}
}

// Ensure the cycle-detector arms surface the participating service in
// a multi-step cycle (A→B→C→A).
func TestTopologicalSort_LongerCycle(t *testing.T) {
	plans := map[string]*Plan{
		"a": {Service: "a", DependsOn: []string{"b"}},
		"b": {Service: "b", DependsOn: []string{"c"}},
		"c": {Service: "c", DependsOn: []string{"a"}},
	}
	_, err := TopologicalSort(plans)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got: %v", err)
	}
}

// -- nclDataHome with no XDG_DATA_HOME -------------------------------------

// TestDefaultRootfsPath_NoXDG exercises the fallback to ~/.local/share.
// Use t.Setenv to remove XDG_DATA_HOME and rely on the home-dir lookup.
func TestDefaultRootfsPath_NoXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	p := &Plan{OCIImage: "registry/img:tag"}
	got := p.DefaultRootfsPath()
	if !strings.Contains(got, ".local/share/ncl/images/") {
		t.Errorf("DefaultRootfsPath fallback = %q", got)
	}
}

// TestListServices_SkipsRegularFiles ensures a regular file in the infra
// directory is ignored.
func TestListServices_SkipsRegularFiles(t *testing.T) {
	root := t.TempDir()
	infraDir := filepath.Join(root, "infra")
	if err := os.MkdirAll(infraDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One real plan dir, one stray file.
	if err := os.MkdirAll(filepath.Join(infraDir, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infraDir, "svc", "plan.hcl"), []byte(`service "svc" { oci_image = "x" }`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infraDir, "README"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ListServices(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "svc" {
		t.Errorf("services = %v, want [svc] (regular file must be skipped)", got)
	}
}

// TestMaterialiseConfigFile_RenameFailure exercises the rename-error
// branch: pre-create the target as a directory, so os.Rename fails
// when trying to overwrite it.
func TestMaterialiseConfigFile_RenameFailure(t *testing.T) {
	scratch := t.TempDir()
	p := &Plan{
		Service: "svc",
		ConfigFile: &ConfigFileBlk{
			Path:     "/etc/svc.conf",
			Template: "ok",
		},
	}
	// Pre-create the target path as a directory so rename fails.
	dir := filepath.Join(scratch, "svc")
	if err := os.MkdirAll(filepath.Join(dir, "svc.conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := MaterialiseConfigFile(p, scratch, SingleReplicaContext(p))
	if err == nil {
		t.Fatal("expected rename failure when target path is a directory")
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Errorf("error should mention rename, got: %v", err)
	}
}

