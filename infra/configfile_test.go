package infra

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaterialiseConfigFile_NilBlock pins the no-op case: a plan
// without a config_file block returns ("", nil), no error, no
// filesystem writes. deployPlan relies on the empty-string
// signal to decide whether to append the share.
func TestMaterialiseConfigFile_NilBlock(t *testing.T) {
	p := &Plan{Service: "nope"}
	scratch := t.TempDir()
	got, err := MaterialiseConfigFile(p, scratch, SingleReplicaContext(p))
	if err != nil {
		t.Fatalf("MaterialiseConfigFile: %v", err)
	}
	if got != "" {
		t.Errorf("returned path %q for plan with no config_file, want empty", got)
	}
	entries, _ := os.ReadDir(scratch)
	if len(entries) != 0 {
		t.Errorf("scratch root touched (%d entries) for plan with no config_file", len(entries))
	}
}

// TestMaterialiseConfigFile_WritesTemplate is the happy path: the
// rendered file lands at <scratch>/<service>/<basename>, mode
// 0600, with the exact template content.
func TestMaterialiseConfigFile_WritesTemplate(t *testing.T) {
	p := &Plan{
		Service: "nats",
		ConfigFile: &ConfigFileBlk{
			Path:     "/etc/nats/nats.conf",
			Template: "server_name: \"nats\"\nlisten: \"0.0.0.0:4222\"\n",
		},
	}
	scratch := t.TempDir()
	got, err := MaterialiseConfigFile(p, scratch, SingleReplicaContext(p))
	if err != nil {
		t.Fatalf("MaterialiseConfigFile: %v", err)
	}
	wantDir := filepath.Join(scratch, "nats")
	if got != wantDir {
		t.Errorf("returned dir = %q, want %q", got, wantDir)
	}
	wantFile := filepath.Join(wantDir, "nats.conf")
	info, err := os.Stat(wantFile)
	if err != nil {
		t.Fatalf("stat %s: %v", wantFile, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	body, err := os.ReadFile(wantFile)
	if err != nil {
		t.Fatalf("read %s: %v", wantFile, err)
	}
	if string(body) != p.ConfigFile.Template {
		t.Errorf("content mismatch:\n got: %q\nwant: %q", body, p.ConfigFile.Template)
	}
}

// TestMaterialiseConfigFile_TokensSubstitutedSingleReplica
// confirms the substitution contract for the single-replica
// deployer: $REPLICA = 1, $DC = "dc1", $PRIVATE_IP from the
// plan's first static IP, $PEERS empty. Operator-side secrets
// ($BASE_DOMAIN, $ADMIN_BCRYPT_HASH) survive verbatim.
func TestMaterialiseConfigFile_TokensSubstitutedSingleReplica(t *testing.T) {
	const tmpl = "node: $REPLICA dc: $DC ip: $PRIVATE_IP peers: $PEERS domain: $BASE_DOMAIN"
	p := &Plan{
		Service: "etcd",
		Network: &NetworkBlk{
			Name:     "control-plane",
			StaticIP: []string{"10.255.1.10", "10.255.1.11", "10.255.1.12"},
		},
		ConfigFile: &ConfigFileBlk{
			Path:     "/etc/etcd/etcd.conf",
			Template: tmpl,
		},
	}
	got, _ := MaterialiseConfigFile(p, t.TempDir(), SingleReplicaContext(p))
	body, _ := os.ReadFile(filepath.Join(got, "etcd.conf"))
	want := "node: 1 dc: dc1 ip: 10.255.1.10 peers:  domain: $BASE_DOMAIN"
	if string(body) != want {
		t.Errorf("substitution mismatch:\n got: %q\nwant: %q", body, want)
	}
}

// TestBuildReplicaContext_MultiReplica pins the fan-out path: a
// 3-replica plan with 3 static IPs produces per-replica contexts
// with $DC, $PRIVATE_IP, $PEERS, and $PEER_DC all reflecting the
// replica's position. Peers is "everything but mine"; PeerDC
// rotates DC1→DC2, DC2→DC3, DC3→DC1.
func TestBuildReplicaContext_MultiReplica(t *testing.T) {
	p := &Plan{
		Service: "etcd",
		Network: &NetworkBlk{
			StaticIP: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		},
		Placement: &PlacementBlk{Count: 3},
	}
	cases := []struct {
		replica int
		dc      string
		ip      string
		peers   []string
		peerDC  string
	}{
		{1, "dc1", "10.0.0.1", []string{"10.0.0.2", "10.0.0.3"}, "dc2"},
		{2, "dc2", "10.0.0.2", []string{"10.0.0.1", "10.0.0.3"}, "dc3"},
		{3, "dc3", "10.0.0.3", []string{"10.0.0.1", "10.0.0.2"}, "dc1"},
	}
	for _, c := range cases {
		ctx := BuildReplicaContext(p, c.replica)
		if ctx.DC != c.dc {
			t.Errorf("replica %d: DC = %q, want %q", c.replica, ctx.DC, c.dc)
		}
		if ctx.PrivateIP != c.ip {
			t.Errorf("replica %d: PrivateIP = %q, want %q", c.replica, ctx.PrivateIP, c.ip)
		}
		if ctx.PeerDC != c.peerDC {
			t.Errorf("replica %d: PeerDC = %q, want %q", c.replica, ctx.PeerDC, c.peerDC)
		}
		if !equalSlices(ctx.Peers, c.peers) {
			t.Errorf("replica %d: Peers = %v, want %v", c.replica, ctx.Peers, c.peers)
		}
	}
}

func equalSlices(a, b []string) bool {
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

// TestMaterialiseConfigFile_PerReplicaSubdir confirms multi-replica
// deploys write to per-replica scratch subdirs so they don't
// clash on the same file path.
func TestMaterialiseConfigFile_PerReplicaSubdir(t *testing.T) {
	p := &Plan{
		Service:   "etcd",
		Placement: &PlacementBlk{Count: 3},
		ConfigFile: &ConfigFileBlk{
			Path:     "/etc/etcd/etcd.conf",
			Template: "node: $REPLICA",
		},
	}
	scratch := t.TempDir()
	for r := 1; r <= 3; r++ {
		dir, err := MaterialiseConfigFile(p, scratch, BuildReplicaContext(p, r))
		if err != nil {
			t.Fatalf("replica %d: %v", r, err)
		}
		want := filepath.Join(scratch, fmt.Sprintf("etcd-dc%d", r))
		if dir != want {
			t.Errorf("replica %d: dir = %q, want %q", r, dir, want)
		}
		body, _ := os.ReadFile(filepath.Join(dir, "etcd.conf"))
		wantBody := fmt.Sprintf("node: %d", r)
		if string(body) != wantBody {
			t.Errorf("replica %d: body = %q, want %q", r, body, wantBody)
		}
	}
}

// TestBuildReplicaContextWithHost_AZOverride pins the
// scheduler-aware path : when the deployer passes a non-empty
// AZ (the picked host's AZ name), it replaces the synthetic
// `dc<i>` label in the rendered context. Empty falls back to
// `dc<i>` — the single-host all-in-one default.
func TestBuildReplicaContextWithHost_AZOverride(t *testing.T) {
	p := &Plan{Service: "etcd", Placement: &PlacementBlk{Count: 3}}
	cases := []struct {
		replica int
		az      string
		wantDC  string
	}{
		{1, "us-east-1a", "us-east-1a"},
		{2, "us-east-1b", "us-east-1b"},
		{3, "", "dc3"}, // empty AZ → synthetic fallback
		{1, "", "dc1"},
	}
	for _, c := range cases {
		ctx := BuildReplicaContextWithHost(p, c.replica, c.az)
		if ctx.DC != c.wantDC {
			t.Errorf("replica=%d az=%q: DC = %q, want %q", c.replica, c.az, ctx.DC, c.wantDC)
		}
	}
}

// TestPlanGroupScheduleRequest_RendersPlacement pins the bridge
// between the HCL placement block and weft.GroupScheduleRequest.
// A `placement { count, az, rack, host }` block maps to a
// proximity-3-tuple ; missing or unknown proximities default to
// ProximityAny.
func TestPlanGroupScheduleRequest_RendersPlacement(t *testing.T) {
	p := &Plan{
		Service: "nats",
		Project: "infra",
		Placement: &PlacementBlk{
			Count: 3,
			AZ:    "different",
			Rack:  "different",
			Host:  "different",
		},
	}
	req, err := p.GroupScheduleRequest("infra-uuid", "apple-vz")
	if err != nil {
		t.Fatalf("GroupScheduleRequest: %v", err)
	}
	if req.Replicas != 3 {
		t.Errorf("Replicas = %d, want 3", req.Replicas)
	}
	if req.ProjectUUID != "infra-uuid" || req.Hypervisor != "apple-vz" {
		t.Errorf("per-replica fields: %+v", req.ScheduleRequest)
	}
	if string(req.Placement.AZ) != "different" ||
		string(req.Placement.Rack) != "different" ||
		string(req.Placement.Host) != "different" {
		t.Errorf("placement = (%q, %q, %q), want all=different",
			req.Placement.AZ, req.Placement.Rack, req.Placement.Host)
	}
}

// TestPlanGroupScheduleRequest_NilPlacement covers the
// single-replica default : no placement block → Replicas=1,
// rule = all-any.
func TestPlanGroupScheduleRequest_NilPlacement(t *testing.T) {
	p := &Plan{Service: "dex"}
	req, err := p.GroupScheduleRequest("infra-uuid", "apple-vz")
	if err != nil {
		t.Fatalf("GroupScheduleRequest: %v", err)
	}
	if req.Replicas != 1 {
		t.Errorf("Replicas = %d, want 1 (no placement block)", req.Replicas)
	}
	if string(req.Placement.AZ) != "" || string(req.Placement.Rack) != "" || string(req.Placement.Host) != "" {
		t.Errorf("placement = %+v, want all-empty (ProximityAny)", req.Placement)
	}
}

// TestVMNameFor pins the naming contract: count<=1 keeps the
// legacy "infra-<service>" shape, count>1 suffixes with -dc<i>.
func TestVMNameFor(t *testing.T) {
	single := &Plan{Service: "etcd"}
	if got := single.VMNameFor(1); got != "infra-etcd" {
		t.Errorf("single-replica VMNameFor(1) = %q, want infra-etcd", got)
	}
	multi := &Plan{Service: "etcd", Placement: &PlacementBlk{Count: 3}}
	want := []string{"infra-etcd-dc1", "infra-etcd-dc2", "infra-etcd-dc3"}
	for i, w := range want {
		if got := multi.VMNameFor(i + 1); got != w {
			t.Errorf("multi VMNameFor(%d) = %q, want %q", i+1, got, w)
		}
	}
}

// TestRenderTemplate_WordBoundary pins the regex-bounded matcher:
// `$DC` must not substitute inside `$DCFOO` or `$DC_extra`, and
// `$PEER_DC` must not match a longer `$PEER_DCX`. Catches the
// "naive string-replace eats neighbouring identifiers" bug.
func TestRenderTemplate_WordBoundary(t *testing.T) {
	ctx := TemplateContext{Replica: 1, DC: "X", PrivateIP: "Y", PeerDC: "Z"}
	cases := []struct {
		in   string
		want string
	}{
		{"$DC", "X"},
		{"$DCFOO", "$DCFOO"},      // word continues — don't match
		{"$DC_extra", "$DC_extra"}, // _ is a word char — don't match
		{"$DC,", "X,"},             // comma is non-word — match
		{"$DC ", "X "},             // space — match
		{"prefix$DCsuffix", "prefix$DCsuffix"}, // identifier continues
		{"$PEER_DC ", "Z "},
		{"$PEER_DCX", "$PEER_DCX"}, // not the same token
	}
	for _, c := range cases {
		if got := RenderTemplate(c.in, ctx); got != c.want {
			t.Errorf("RenderTemplate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRenderTemplate_PeersJoin pins the Peers→$PEERS encoding:
// the slice is comma-joined verbatim into the template.
func TestRenderTemplate_PeersJoin(t *testing.T) {
	ctx := TemplateContext{Peers: []string{"a.example", "b.example", "c.example"}}
	got := RenderTemplate("peers: [$PEERS]", ctx)
	want := "peers: [a.example,b.example,c.example]"
	if got != want {
		t.Errorf("RenderTemplate peers join: got %q, want %q", got, want)
	}
}

// TestMaterialiseConfigFile_PathFallback covers degenerate
// ConfigFile.Path values ("", ".", "/") — the helper falls back
// to a "config" basename rather than failing or writing into a
// weird location.
func TestMaterialiseConfigFile_PathFallback(t *testing.T) {
	for _, badPath := range []string{"", ".", "/"} {
		p := &Plan{
			Service: "svc",
			ConfigFile: &ConfigFileBlk{
				Path:     badPath,
				Template: "hello",
			},
		}
		got, err := MaterialiseConfigFile(p, t.TempDir(), SingleReplicaContext(p))
		if err != nil {
			t.Errorf("MaterialiseConfigFile(path=%q): %v", badPath, err)
			continue
		}
		entries, _ := os.ReadDir(got)
		if len(entries) != 1 || entries[0].Name() != "config" {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("path=%q produced entries %v, want [config]", badPath, names)
		}
		// Sanity: file content survives the fallback path too.
		body, _ := os.ReadFile(filepath.Join(got, "config"))
		if !strings.Contains(string(body), "hello") {
			t.Errorf("path=%q dropped template content: %q", badPath, body)
		}
	}
}
