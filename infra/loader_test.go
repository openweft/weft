package infra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTopologicalSort_LinearChain pins the canonical infra layout:
// etcd → dex → zot, dex → nats. Expected order: etcd, dex, then
// nats+zot (lexical tiebreak puts nats before zot in the rendered
// slice).
func TestTopologicalSort_LinearChain(t *testing.T) {
	plans := map[string]*Plan{
		"etcd": {Service: "etcd"},
		"dex":  {Service: "dex", DependsOn: []string{"etcd"}},
		"zot":  {Service: "zot", DependsOn: []string{"dex"}},
		"nats": {Service: "nats", DependsOn: []string{"dex"}},
	}
	out, err := TopologicalSort(plans)
	if err != nil {
		t.Fatalf("TopologicalSort: %v", err)
	}
	got := make([]string, len(out))
	for i, p := range out {
		got[i] = p.Service
	}
	// Two valid orderings exist (nats vs zot order); the
	// implementation picks lexical for determinism.
	want := []string{"etcd", "dex", "nats", "zot"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestTopologicalSort_NoDependencies pins the independent-set case:
// every service has empty DependsOn, output order = lexical input
// order. Critical because most early-bootstrap services land here.
func TestTopologicalSort_NoDependencies(t *testing.T) {
	plans := map[string]*Plan{
		"c": {Service: "c"},
		"a": {Service: "a"},
		"b": {Service: "b"},
	}
	out, err := TopologicalSort(plans)
	if err != nil {
		t.Fatalf("TopologicalSort: %v", err)
	}
	got := []string{out[0].Service, out[1].Service, out[2].Service}
	want := []string{"a", "b", "c"}
	if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestTopologicalSort_UnknownDep pins the typo-protection: a
// depends_on that names a service with no plan errors out with
// the offending names in the message so the operator sees both
// sides of the broken edge.
func TestTopologicalSort_UnknownDep(t *testing.T) {
	plans := map[string]*Plan{
		"a": {Service: "a", DependsOn: []string{"ghost"}},
	}
	_, err := TopologicalSort(plans)
	if err == nil {
		t.Fatal("expected error for unknown dependency, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "a") {
		t.Errorf("error should mention both 'a' and 'ghost', got: %v", err)
	}
}

// TestTopologicalSort_Cycle pins cycle detection. Two services
// pointing at each other must error out — silent acceptance would
// produce an arbitrary deploy order and a hidden infinite-wait.
func TestTopologicalSort_Cycle(t *testing.T) {
	plans := map[string]*Plan{
		"a": {Service: "a", DependsOn: []string{"b"}},
		"b": {Service: "b", DependsOn: []string{"a"}},
	}
	_, err := TopologicalSort(plans)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention 'cycle', got: %v", err)
	}
}

// TestListServices_DiscoversPlanDirs builds a fake infra tree and
// checks ListServices picks up exactly the directories that carry
// a plan.hcl (and ignores empty / file-less dirs).
func TestListServices_DiscoversPlanDirs(t *testing.T) {
	root := t.TempDir()
	infraDir := filepath.Join(root, "infra")
	if err := os.MkdirAll(filepath.Join(infraDir, "etcd"), 0o755); err != nil {
		t.Fatalf("mkdir etcd: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(infraDir, "dex"), 0o755); err != nil {
		t.Fatalf("mkdir dex: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(infraDir, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infraDir, "etcd", "plan.hcl"), []byte(`service "etcd" { oci_image = "x" }`), 0o600); err != nil {
		t.Fatalf("write etcd plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infraDir, "dex", "plan.hcl"), []byte(`service "dex" { oci_image = "x" }`), 0o600); err != nil {
		t.Fatalf("write dex plan: %v", err)
	}

	got, err := ListServices(root)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	want := []string{"dex", "etcd"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("services = %v, want %v (empty dir must be skipped)", got, want)
	}
}

// TestPlacementBlk_ParsesFromHCL builds a synthetic plan with a
// full `placement { count, az, rack, host }` block and confirms
// LoadPlan threads the values onto the decoded Plan. Catches
// drift between the HCL schema tags and the Go struct.
func TestPlacementBlk_ParsesFromHCL(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "infra", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `service "demo" {
  oci_image = "x"
  placement {
    count = 3
    az    = "different"
    rack  = "different"
    host  = "different"
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "plan.hcl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	p, err := LoadPlan(DefaultPlanPath(root, "demo"))
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if p.Placement == nil {
		t.Fatal("placement block not decoded")
	}
	if p.Placement.Count != 3 || p.Placement.AZ != "different" || p.Placement.Rack != "different" || p.Placement.Host != "different" {
		t.Errorf("placement = %+v, want {3 different different different}", *p.Placement)
	}
	if got := p.ReplicaCount(); got != 3 {
		t.Errorf("ReplicaCount() = %d, want 3", got)
	}
}

// TestPlacementBlk_DefaultReplicaCount pins the "no block →
// replicas=1" convenience that deployPlan relies on.
func TestPlacementBlk_DefaultReplicaCount(t *testing.T) {
	cases := []struct {
		name string
		plan *Plan
		want int
	}{
		{"nil plan", nil, 1},
		{"no block", &Plan{}, 1},
		{"block with zero count", &Plan{Placement: &PlacementBlk{}}, 1},
		{"explicit count", &Plan{Placement: &PlacementBlk{Count: 5}}, 5},
	}
	for _, c := range cases {
		if got := c.plan.ReplicaCount(); got != c.want {
			t.Errorf("%s: ReplicaCount() = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestPlacementBlk_RejectsInvalidProximity catches operator typos
// (`az = "differnt"` etc.) at LoadPlan time so they don't reach
// the scheduler. The error names the offending field + value so
// the operator can navigate the plan file.
func TestPlacementBlk_RejectsInvalidProximity(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  string // substring of the expected error
	}{
		{"bad az", "placement {\n  az = \"differnt\"\n}", "placement.az"},
		{"bad rack", "placement {\n  rack = \"no\"\n}", "placement.rack"},
		{"bad host", "placement {\n  host = \"anywhere\"\n}", "placement.host"},
		{"negative count", "placement {\n  count = -1\n}", "count must be >= 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "infra", "demo")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			body := "service \"demo\" {\n  oci_image = \"x\"\n  " + c.body + "\n}\n"
			if err := os.WriteFile(filepath.Join(dir, "plan.hcl"), []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := LoadPlan(DefaultPlanPath(root, "demo"))
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q missing %q", err.Error(), c.want)
			}
		})
	}
}

// TestLoadAllPlans_PlanLabelMismatch pins the typo-protection at
// load time: a plan whose service label doesn't match its
// directory name errors with both names so the operator sees
// which side they need to fix.
func TestLoadAllPlans_PlanLabelMismatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "infra", "intended-name")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.hcl"), []byte(`service "actual-name" { oci_image = "x" }`), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	_, err := LoadAllPlans(root, []string{"intended-name"})
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "intended-name") || !strings.Contains(err.Error(), "actual-name") {
		t.Errorf("error should mention both names, got: %v", err)
	}
}
