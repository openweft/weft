package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpand_VersionPlaceholder(t *testing.T) {
	got := expand("docker pull ghcr.io/openweft/weft:{{.Version}}", "v0.4.52")
	want := "docker pull ghcr.io/openweft/weft:v0.4.52"
	if got != want {
		t.Errorf("expand = %q, want %q", got, want)
	}
	// Empty version still produces a deterministic string — the
	// validation lives in runUpgrade, not here.
	if got := expand("foo {{.Version}}", ""); got != "foo " {
		t.Errorf("empty-version expand = %q", got)
	}
}

func TestHostShortName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"admin@dc1-r1-h1", "dc1-r1-h1"},
		{"dc2-r1-h1", "dc2-r1-h1"},
		{"admin@dc3-r1-h1:2222", "dc3-r1-h1"},
		{"root@1.2.3.4:22", "1.2.3.4"},
	}
	for _, c := range cases {
		if got := hostShortName(c.in); got != c.want {
			t.Errorf("hostShortName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHostsFromClusterHCL(t *testing.T) {
	dir := t.TempDir()
	hcl := `
cluster "prod" {
  host "dc1-r1-h1" {
    addr = "admin@dc1-r1-h1"
    role = "control-plane"
  }
  host "dc2-r1-h1" {
    addr = "admin@dc2-r1-h1"
  }
  host "dc3-r1-h1" {
    addr = "admin@dc3-r1-h1"
  }
}
`
	path := filepath.Join(dir, "cluster.hcl")
	if err := os.WriteFile(path, []byte(hcl), 0o644); err != nil {
		t.Fatal(err)
	}
	hosts, err := hostsFromClusterHCL(path)
	if err != nil {
		t.Fatalf("hostsFromClusterHCL: %v", err)
	}
	want := []string{"admin@dc1-r1-h1", "admin@dc2-r1-h1", "admin@dc3-r1-h1"}
	if len(hosts) != len(want) {
		t.Fatalf("got %d hosts, want %d : %v", len(hosts), len(want), hosts)
	}
	for i, h := range want {
		if hosts[i] != h {
			t.Errorf("hosts[%d] = %q, want %q", i, hosts[i], h)
		}
	}
}

func TestRunUpgrade_MissingFlags(t *testing.T) {
	// --to missing
	err := runUpgrade(nil, options{Out: os.Stderr})
	if err == nil || !strings.Contains(err.Error(), "--to") {
		t.Errorf("expected --to error, got %v", err)
	}
	// hosts + cluster-hcl both empty
	err = runUpgrade(nil, options{Version: "v1", Out: os.Stderr})
	if err == nil || !strings.Contains(err.Error(), "--host") {
		t.Errorf("expected --host error, got %v", err)
	}
}

func TestRunUpgrade_DowngradeRefusedWithoutYes(t *testing.T) {
	err := runUpgrade(nil, options{
		Version:   "v0.4.10",
		Hosts:     []string{"admin@h1"},
		Downgrade: true,
		Out:       os.Stderr,
	})
	if err == nil || !strings.Contains(err.Error(), "refused without --yes") {
		t.Errorf("downgrade should refuse without --yes ; got %v", err)
	}
}

func TestRunUpgrade_DryRunWalksAllHosts(t *testing.T) {
	// Capture output so we can assert the per-host phase markers
	// appeared in order.
	f, err := os.CreateTemp(t.TempDir(), "out-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	err = runUpgrade(nil, options{
		Version: "v0.4.52",
		Hosts:   []string{"admin@h1", "admin@h2", "admin@h3"},
		DryRun:  true,
		Soak:    0,
		Out:     f,
	})
	if err != nil {
		t.Fatalf("dry-run upgrade: %v", err)
	}
	// Read back the log.
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"phase 1/3 : admin@h1",
		"phase 2/3 : admin@h2",
		"phase 3/3 : admin@h3",
		"→ cordon",
		"→ image-pull",
		"→ install",
		"→ restart",
		"→ wait-ready",
		"→ uncordon",
		"all 3 host(s) on v0.4.52 — done",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q ; full :\n%s", want, got)
		}
	}
}
