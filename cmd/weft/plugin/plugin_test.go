package plugin

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/internal/testutil"
)

func strPtr(s string) *string { return &s }

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	return <-done
}

// findCatalogue walks up to locate the shipped catalogue/ directory.
// The tests run from cmd/weft/plugin/ so the search bubbles up two
// levels to the repo root.
func findCatalogue(t *testing.T) string {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for cur := wd; cur != "/" && cur != "."; cur = filepath.Dir(cur) {
		c := filepath.Join(cur, "catalogue")
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	t.Fatalf("catalogue not found from %s", wd)
	return ""
}

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "plugin" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := map[string]bool{"list": false, "install": false, "uninstall": false, "status": false}
	for _, c := range cmd.Commands() {
		name := strings.SplitN(c.Use, " ", 2)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("missing subcommand %q", n)
		}
	}
}

func TestList_RendersTable(t *testing.T) {
	cat := findCatalogue(t)
	stateDir := t.TempDir()
	out := captureStdout(t, func() {
		cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"list", "--catalogue", cat, "--state-dir", stateDir})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if !strings.Contains(out, "gitlab-runners-ha") {
		t.Errorf("output missing gitlab plugin: %q", out)
	}
	if !strings.Contains(out, "NAME") {
		t.Errorf("output missing header: %q", out)
	}
}

func TestList_JSON(t *testing.T) {
	cat := findCatalogue(t)
	stateDir := t.TempDir()
	out := captureStdout(t, func() {
		cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"list", "--catalogue", cat, "--state-dir", stateDir, "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name": "gitlab-runners-ha"`) {
		t.Errorf("JSON missing plugin: %q", out)
	}
}

func TestInstall_MissingRequiredInput(t *testing.T) {
	cat := findCatalogue(t)
	stateDir := t.TempDir()
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"install", "gitlab-runners-ha",
		"--catalogue", cat,
		"--state-dir", stateDir,
		"--dry-run",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing required input") {
		t.Fatalf("expected missing-required-input error, got %v", err)
	}
}

func TestInstall_DryRunSuccess(t *testing.T) {
	cat := findCatalogue(t)
	stateDir := t.TempDir()
	out := captureStdout(t, func() {
		cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"install", "gitlab-runners-ha",
			"--catalogue", cat,
			"--state-dir", stateDir,
			"--project", "ci",
			"--input", "registration_token=glrt-test",
			"--dry-run",
		})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if !strings.Contains(out, "dry-run") {
		t.Errorf("missing dry-run banner: %q", out)
	}
	if !strings.Contains(out, "total VMs: 3") {
		t.Errorf("expected 3 VMs in dry-run, got %q", out)
	}
	// Secret must be masked in dry-run.
	if strings.Contains(out, "glrt-test") {
		t.Errorf("dry-run leaked secret token: %q", out)
	}
	if !strings.Contains(out, "registration_token=***") {
		t.Errorf("expected secret to be displayed as ***, got %q", out)
	}
}

func TestInstall_UnknownPlugin(t *testing.T) {
	cat := findCatalogue(t)
	stateDir := t.TempDir()
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"install", "ghost-plugin",
		"--catalogue", cat,
		"--state-dir", stateDir,
		"--dry-run",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestInstall_BadInputFormat(t *testing.T) {
	cat := findCatalogue(t)
	stateDir := t.TempDir()
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"install", "gitlab-runners-ha",
		"--catalogue", cat,
		"--state-dir", stateDir,
		"--input", "no-equals-sign",
		"--dry-run",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid --input") {
		t.Fatalf("expected invalid-input error, got %v", err)
	}
}

func TestInstall_EndToEnd_AgainstFakeAgent(t *testing.T) {
	cat := findCatalogue(t)
	stateDir := t.TempDir()
	srv := testutil.NewServer(t)
	// Wire stubs so CreateVM/CreateNetwork/CreateSecurityGroup
	// return non-nil resources.
	var vmCalls, netCalls, sgCalls int
	srv.CreateNetworkFn = func(_ context.Context, in *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error) {
		netCalls++
		return &weftv1.CreateNetworkResponse{Network: &weftv1.NetworkInfo{Uuid: "net-" + in.Name, Name: in.Name, Cidr: in.Cidr}}, nil
	}
	srv.CreateSecurityGroupFn = func(_ context.Context, in *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error) {
		sgCalls++
		return &weftv1.CreateSecurityGroupResponse{Group: &weftv1.SecurityGroupInfo{Uuid: "sg-" + in.Name, Name: in.Name}}, nil
	}
	srv.CreateVMFn = func(_ context.Context, in *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error) {
		vmCalls++
		_ = in
		return &weftv1.CreateVMResponse{}, nil
	}
	srv.CreateVolumeFn = func(_ context.Context, in *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error) {
		return &weftv1.CreateVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: "vol-" + in.Name, Name: in.Name, SizeGib: in.SizeGib}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"install", "github-runners-ha",
			"--catalogue", cat,
			"--state-dir", stateDir,
			"--project", "ci",
			"--input", "github_pat=ghp_test",
			"--input", "github_url=https://github.com/openweft",
		})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if !strings.Contains(out, "installed") {
		t.Errorf("missing installed banner: %q", out)
	}
	if vmCalls != 3 {
		t.Errorf("expected 3 CreateVM, got %d", vmCalls)
	}
	if netCalls != 1 {
		t.Errorf("expected 1 CreateNetwork, got %d", netCalls)
	}
	if sgCalls != 1 {
		t.Errorf("expected 1 CreateSG, got %d", sgCalls)
	}
}

func TestStatus_EmptyStateDir(t *testing.T) {
	stateDir := t.TempDir()
	out := captureStdout(t, func() {
		cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"status", "--state-dir", stateDir})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if !strings.Contains(out, "NAME") {
		t.Errorf("expected header even for empty state, got %q", out)
	}
}

func TestUninstall_NoInstance(t *testing.T) {
	stateDir := t.TempDir()
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"uninstall", "gitlab-runners-ha", "--state-dir", stateDir})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no installed instances") {
		t.Fatalf("expected no-installed-instances error, got %v", err)
	}
}
