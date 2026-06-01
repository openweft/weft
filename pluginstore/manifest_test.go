package pluginstore

import (
	"path/filepath"
	"strings"
	"testing"
)

const minimalManifest = `
plugin "demo" {
  version     = "v1"
  kind        = "runner-farm"
  description = "demo plugin"
  layout      = "ha-3dc"

  input "token" {
    type     = "string"
    required = true
    secret   = true
  }

  network "n1" {
    cidr = "10.0.0.0/24"
  }

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
    replicas = 3
    cpu      = 2
    mem_mb   = 4096
    disk_gb  = 20
    network  = "n1"
    placement { az = "different" }
    env_from "token" { env_name = "TOKEN" }
    volume "cache" { size_gib = 10 }
  }
}
`

// ── Schema parsing & validation ──────────────────────────────────

func TestParseManifest_Minimal(t *testing.T) {
	m, err := ParseManifest("demo.hcl", []byte(minimalManifest))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Name != "demo" {
		t.Errorf("name = %q", m.Name)
	}
	if m.Layout != "ha-3dc" {
		t.Errorf("layout = %q", m.Layout)
	}
	if len(m.VMs) != 1 || m.VMs[0].Replicas != 3 {
		t.Errorf("vm replicas = %+v", m.VMs)
	}
	if len(m.Networks) != 1 || m.Networks[0].CIDR != "10.0.0.0/24" {
		t.Errorf("net = %+v", m.Networks)
	}
	if len(m.SecurityGroups) != 1 || len(m.SecurityGroups[0].Rules) != 1 {
		t.Errorf("sg = %+v", m.SecurityGroups)
	}
	if m.SecurityGroups[0].Rules[0].Direction != "egress" {
		t.Errorf("rule direction = %q", m.SecurityGroups[0].Rules[0].Direction)
	}
	if !m.Inputs[0].Secret {
		t.Errorf("input secret flag dropped")
	}
}

func TestParseManifest_MissingVersion(t *testing.T) {
	src := `
plugin "demo" {
  kind = "x"
  description = "d"
  vm "v" { image = "i" }
}`
	_, err := ParseManifest("demo.hcl", []byte(src))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("expected version error, got %v", err)
	}
}

func TestParseManifest_UnsupportedVersion(t *testing.T) {
	src := `
plugin "demo" {
  version = "v999"
  kind = "x"
  description = "d"
  vm "v" { image = "i" }
}`
	_, err := ParseManifest("demo.hcl", []byte(src))
	if err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("expected unsupported-version error, got %v", err)
	}
}

func TestParseManifest_UnknownNetworkRef(t *testing.T) {
	src := `
plugin "demo" {
  version = "v1"
  kind = "x"
  description = "d"
  vm "v" {
    image   = "i"
    network = "ghost"
  }
}`
	_, err := ParseManifest("demo.hcl", []byte(src))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected unknown-network error, got %v", err)
	}
}

func TestParseManifest_BadRuleDirection(t *testing.T) {
	src := `
plugin "demo" {
  version = "v1"
  kind = "x"
  description = "d"
  network "n" { cidr = "10.0.0.0/24" }
  security_group "sg" {
    networks = ["n"]
    rule "sideways" { protocol = "tcp" }
  }
  vm "v" { image = "i" }
}`
	_, err := ParseManifest("demo.hcl", []byte(src))
	if err == nil || !strings.Contains(err.Error(), "direction") {
		t.Fatalf("expected direction error, got %v", err)
	}
}

// ── Inputs ───────────────────────────────────────────────────────

func TestValidateInputs_MissingRequired(t *testing.T) {
	m := &Manifest{
		Name: "demo",
		Inputs: []Input{
			{Name: "token", Required: true},
		},
	}
	if _, err := m.ValidateInputs(nil); err == nil || !strings.Contains(err.Error(), "missing required input") {
		t.Fatalf("expected missing-required error, got %v", err)
	}
}

func TestValidateInputs_DefaultsFilled(t *testing.T) {
	m := &Manifest{
		Name: "demo",
		Inputs: []Input{
			{Name: "url", Default: "https://example.com"},
			{Name: "token", Required: true},
		},
	}
	got, err := m.ValidateInputs(map[string]any{"token": "abc"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got["url"] != "https://example.com" || got["token"] != "abc" {
		t.Errorf("resolved = %+v", got)
	}
}

func TestValidateInputs_UnknownKeyRejected(t *testing.T) {
	m := &Manifest{Name: "demo"}
	if _, err := m.ValidateInputs(map[string]any{"ghost": "x"}); err == nil || !strings.Contains(err.Error(), "unknown input") {
		t.Fatalf("expected unknown-input error, got %v", err)
	}
}

// ── Deterministic UUID ───────────────────────────────────────────

func TestDeterministicInstanceUUID_StableAcrossKeyOrder(t *testing.T) {
	a := DeterministicInstanceUUID("demo", "proj", map[string]string{"a": "1", "b": "2"})
	b := DeterministicInstanceUUID("demo", "proj", map[string]string{"b": "2", "a": "1"})
	if a != b {
		t.Errorf("uuid not stable across key order: %q vs %q", a, b)
	}
}

func TestDeterministicInstanceUUID_DiffersByInputs(t *testing.T) {
	a := DeterministicInstanceUUID("demo", "proj", map[string]string{"a": "1"})
	b := DeterministicInstanceUUID("demo", "proj", map[string]string{"a": "2"})
	if a == b {
		t.Errorf("uuid collides for distinct inputs: %q", a)
	}
}

// ── Catalogue end-to-end : parse every shipped plugin.hcl ────────

func TestParseCatalogue_AllShippedPluginsParse(t *testing.T) {
	root := findCatalogueRoot(t)
	cat, err := LoadCatalogue(root)
	if err != nil {
		t.Fatalf("load catalogue: %v", err)
	}
	want := []string{"gitlab-runners-ha", "github-runners-ha", "forgejo-runners-ha"}
	for _, name := range want {
		m, ok := cat[name]
		if !ok {
			t.Errorf("missing plugin %q in catalogue", name)
			continue
		}
		if m.Layout != "ha-3dc" {
			t.Errorf("%s: layout = %q (want ha-3dc)", name, m.Layout)
		}
		if len(m.VMs) == 0 || m.VMs[0].Replicas != 3 {
			t.Errorf("%s: expected 3 replicas, got %+v", name, m.VMs)
		}
		// Each plugin must declare a 'runners' network with a /24.
		if len(m.Networks) == 0 || m.Networks[0].Name != "runners" {
			t.Errorf("%s: expected `runners` network, got %+v", name, m.Networks)
		}
		// Each plugin must declare an egress-only SG.
		if len(m.SecurityGroups) == 0 {
			t.Errorf("%s: expected at least one security group", name)
			continue
		}
		for _, r := range m.SecurityGroups[0].Rules {
			if r.Direction != "egress" {
				t.Errorf("%s: rule has direction %q (want egress)", name, r.Direction)
			}
		}
		// Each plugin pins az=different.
		if m.VMs[0].Placement == nil || m.VMs[0].Placement.AZ != "different" {
			t.Errorf("%s: expected az=different placement, got %+v", name, m.VMs[0].Placement)
		}
	}
}

// findCatalogueRoot walks up from the test cwd until it finds a
// directory named "catalogue" — the same heuristic the runtime
// uses. Tests don't take a flag so this needs to be a closed-form
// search.
func findCatalogueRoot(t *testing.T) string {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for cur := wd; cur != "/" && cur != "."; cur = filepath.Dir(cur) {
		candidate := filepath.Join(cur, "catalogue")
		if entries, err := readDirOK(candidate); err == nil {
			// Must contain at least one of the runner plugins.
			for _, e := range entries {
				if e == "gitlab-runners-ha" || e == "github-runners-ha" || e == "forgejo-runners-ha" {
					return candidate
				}
			}
		}
	}
	t.Fatalf("could not locate catalogue/ from %s", wd)
	return ""
}

func readDirOK(dir string) ([]string, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Base(e))
	}
	return out, nil
}
