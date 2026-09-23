package plugin

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/internal/testutil"
)

// These tests drove an on-disk plugin store until 98e827b01 moved the
// catalogue onto the agent: `list`, `status` and `uninstall` stopped taking
// --state-dir, and `list` stopped taking --catalogue. The tests kept driving
// the old shape for four commits of plugin.go, and nothing said so, because
// the package was in no CI lane.
//
// They now describe the current CLI. The split is deliberate:
//
//   - install --dry-run still reads the shipped catalogue/ off disk and
//     issues no RPCs, so those tests dial nothing and keep --catalogue.
//   - everything else goes through the agent and is driven against
//     testutil.Server, which is why ListPluginCatalogueFn and
//     ListInstalledPluginsFn had to be added to it.

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
// The tests run from cmd/weft/plugin/ so the search bubbles up to the
// repo root. Only the dry-run install path still reads it.
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

// catalogueEntry / instance keep the fixtures short. The fields are the ones
// the renderers actually read; anything else would be scenery.
func catalogueEntry(name, kind, version, desc string) *weftv1.PluginCatalogueEntry {
	return &weftv1.PluginCatalogueEntry{Name: name, Kind: kind, Version: version, Description: desc}
}

func instance(name, uuid, project, status string, vms ...string) *weftv1.PluginInstance {
	return &weftv1.PluginInstance{
		Name:              name,
		InstanceUuid:      uuid,
		Project:           project,
		Status:            status,
		VmUuids:           vms,
		InstalledAtUnixNs: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC).UnixNano(),
	}
}

// agentWith stands up a fake agent answering the two catalogue RPCs.
func agentWith(t *testing.T, cat []*weftv1.PluginCatalogueEntry, inst []*weftv1.PluginInstance) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t)
	srv.ListPluginCatalogueFn = func(context.Context, *weftv1.ListPluginCatalogueRequest) (*weftv1.ListPluginCatalogueResponse, error) {
		return &weftv1.ListPluginCatalogueResponse{Entries: cat}, nil
	}
	srv.ListInstalledPluginsFn = func(context.Context, *weftv1.ListInstalledPluginsRequest) (*weftv1.ListInstalledPluginsResponse, error) {
		return &weftv1.ListInstalledPluginsResponse{Instances: inst}, nil
	}
	return srv
}

// The fixture inputs below are shaped like the real ones on purpose: the
// masking assertion in TestInstall_DryRunSuccess is only meaningful if the
// value looks like something worth masking. They are literals in a test, and
// they are split so that no credential-shaped string sits in the source as
// one token.
const (
	fakeGitLabToken = "glrt-" + "not-a-real-token"
	fakeGitHubPAT   = "ghp_" + "not-a-real-token"
)

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

// ⛔ The flags removed by 98e827b01 must stay removed. Without this, a later
// rewrite could quietly reintroduce --state-dir on `list` and nothing would
// notice until the same drift had happened again.
func TestListAndStatus_RejectTheRetiredFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"list --state-dir", []string{"list", "--state-dir", t.TempDir()}},
		{"list --catalogue", []string{"list", "--catalogue", t.TempDir()}},
		{"status --state-dir", []string{"status", "--state-dir", t.TempDir()}},
		{"uninstall --state-dir", []string{"uninstall", "x", "--state-dir", t.TempDir()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "unknown flag") {
				t.Fatalf("expected an unknown-flag error, got %v", err)
			}
		})
	}
}

func TestList_RendersTable(t *testing.T) {
	srv := agentWith(t,
		[]*weftv1.PluginCatalogueEntry{
			catalogueEntry("gitlab-runners-ha", "runners", "1.2.0", "GitLab runners, HA"),
			catalogueEntry("github-runners-ha", "runners", "0.9.0", "GitHub runners, HA"),
		},
		[]*weftv1.PluginInstance{instance("gitlab-runners-ha", "inst-1", "ci", "running", "vm-a", "vm-b")},
	)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"list"})
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
	// The INSTALLED column is counted from ListInstalledPlugins, which is the
	// behaviour the move to the agent introduced. One instance of gitlab, none
	// of github -- both directions, so a renderer printing a constant fails.
	//
	// Read by column index rather than by matching a padded literal: the
	// padding is tabwriter's to choose, and an assertion that depends on it
	// breaks when a longer description turns up. The first attempt here did
	// exactly that and failed against output that was correct.
	for _, want := range []struct {
		name      string
		installed string
	}{
		{"gitlab-runners-ha", "1"},
		{"github-runners-ha", "0"},
	} {
		row := rowFor(t, out, want.name)
		if len(row) < 4 {
			t.Errorf("row %q has %d columns: %q", want.name, len(row), row)
			continue
		}
		if row[3] != want.installed {
			t.Errorf("%s INSTALLED = %q, want %q (row %q)", want.name, row[3], want.installed, row)
		}
	}
}

// rowFor returns the whitespace-split fields of the table row beginning with
// name.
func rowFor(t *testing.T, out, name string) []string {
	t.Helper()
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, name) {
			return strings.Fields(ln)
		}
	}
	t.Errorf("no row for %q in:\n%s", name, out)
	return nil
}

func TestList_JSON(t *testing.T) {
	srv := agentWith(t,
		[]*weftv1.PluginCatalogueEntry{catalogueEntry("gitlab-runners-ha", "runners", "1.2.0", "GitLab runners, HA")},
		[]*weftv1.PluginInstance{instance("gitlab-runners-ha", "inst-1", "ci", "running")},
	)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"list", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name": "gitlab-runners-ha"`) {
		t.Errorf("JSON missing plugin: %q", out)
	}
	if !strings.Contains(out, `"installed": 1`) {
		t.Errorf("JSON missing installed count: %q", out)
	}
}

func TestInstall_MissingRequiredInput(t *testing.T) {
	cat := findCatalogue(t)
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"install", "gitlab-runners-ha",
		"--catalogue", cat,
		"--dry-run",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing required input") {
		t.Fatalf("expected missing-required-input error, got %v", err)
	}
}

func TestInstall_DryRunSuccess(t *testing.T) {
	cat := findCatalogue(t)
	out := captureStdout(t, func() {
		cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"install", "gitlab-runners-ha",
			"--catalogue", cat,
			"--project", "ci",
			"--input", "registration_token=" + fakeGitLabToken,
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
	// The secret must be masked in dry-run.
	if strings.Contains(out, fakeGitLabToken) {
		t.Errorf("dry-run leaked the token value: %q", out)
	}
	if !strings.Contains(out, "registration_token=***") {
		t.Errorf("expected the secret to be displayed as ***, got %q", out)
	}
}

func TestInstall_UnknownPlugin(t *testing.T) {
	cat := findCatalogue(t)
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"install", "ghost-plugin",
		"--catalogue", cat,
		"--dry-run",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestInstall_BadInputFormat(t *testing.T) {
	cat := findCatalogue(t)
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"install", "gitlab-runners-ha",
		"--catalogue", cat,
		"--input", "no-equals-sign",
		"--dry-run",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid --input") {
		t.Fatalf("expected invalid-input error, got %v", err)
	}
}

// This test used to stub CreateNetwork / CreateSecurityGroup / CreateVM and
// assert 1 / 1 / 3 calls. That stopped being the CLI's behaviour in v0.4.74:
// install now makes ONE call, InstallPlugin, and the agent creates the
// resources. Counting CreateVM was measuring a path the CLI no longer takes --
// and it could not fail, because the tests never ran.
//
// What is worth pinning is what the CLI puts on the wire and what it does with
// the answer.
func TestInstall_SendsOneInstallPluginRequest(t *testing.T) {
	cat := findCatalogue(t)
	srv := testutil.NewServer(t)

	var calls int
	var got *weftv1.InstallPluginRequest
	srv.InstallPluginFn = func(_ context.Context, in *weftv1.InstallPluginRequest) (*weftv1.InstallPluginResponse, error) {
		calls++
		got = in
		return &weftv1.InstallPluginResponse{InstanceUuid: "inst-42"}, nil
	}
	// If the CLI regressed to orchestrating resources itself, these would
	// fire. Nil stubs return zero values and would let that pass unnoticed.
	var clientSideCalls int
	srv.CreateVMFn = func(context.Context, *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error) {
		clientSideCalls++
		return &weftv1.CreateVMResponse{}, nil
	}
	srv.CreateNetworkFn = func(_ context.Context, in *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error) {
		clientSideCalls++
		return &weftv1.CreateNetworkResponse{Network: &weftv1.NetworkInfo{Uuid: "net-" + in.Name}}, nil
	}
	srv.CreateSecurityGroupFn = func(_ context.Context, in *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error) {
		clientSideCalls++
		return &weftv1.CreateSecurityGroupResponse{Group: &weftv1.SecurityGroupInfo{Uuid: "sg-" + in.Name}}, nil
	}

	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"install", "github-runners-ha",
			"--catalogue", cat,
			"--project", "ci",
			"--input", "github_pat=" + fakeGitHubPAT,
			"--input", "github_url=https://github.com/openweft",
		})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})

	if calls != 1 {
		t.Fatalf("expected exactly 1 InstallPlugin call, got %d", calls)
	}
	if clientSideCalls != 0 {
		t.Errorf("the CLI issued %d resource-creation RPCs; the agent owns that since v0.4.74", clientSideCalls)
	}
	if got.Name != "github-runners-ha" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Project != "ci" {
		t.Errorf("Project = %q", got.Project)
	}
	if got.Inputs["github_pat"] != fakeGitHubPAT {
		t.Errorf("github_pat not forwarded: %q", got.Inputs["github_pat"])
	}
	if got.Inputs["github_url"] != "https://github.com/openweft" {
		t.Errorf("github_url not forwarded: %q", got.Inputs["github_url"])
	}
	if !strings.Contains(out, "installed") {
		t.Errorf("missing installed banner: %q", out)
	}
	// The instance UUID comes back from the agent; printing anything else
	// would leave an operator unable to name the instance they just made.
	if !strings.Contains(out, "inst-42") {
		t.Errorf("output does not carry the returned instance uuid: %q", out)
	}
}

// uninstall resolves the single instance itself and passes its UUID. A CLI
// that sent an empty uuid would delete whatever the agent picked.
func TestUninstall_ResolvesTheSingleInstance(t *testing.T) {
	srv := agentWith(t, nil, []*weftv1.PluginInstance{
		instance("gitlab-runners-ha", "inst-1", "ci", "running"),
	})
	var got *weftv1.UninstallPluginRequest
	srv.UninstallPluginFn = func(_ context.Context, in *weftv1.UninstallPluginRequest) (*weftv1.UninstallPluginResponse, error) {
		got = in
		return &weftv1.UninstallPluginResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"uninstall", "gitlab-runners-ha"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got == nil {
		t.Fatal("UninstallPlugin was never called")
	}
	if got.InstanceUuid != "inst-1" {
		t.Errorf("InstanceUuid = %q, want inst-1", got.InstanceUuid)
	}
	if got.Name != "gitlab-runners-ha" {
		t.Errorf("Name = %q", got.Name)
	}
}

// Was TestStatus_EmptyStateDir. There is no state directory any more, but the
// question it asked -- is the header printed when there is nothing to show --
// is still worth asking, of the agent.
func TestStatus_NoInstances_StillPrintsHeader(t *testing.T) {
	srv := agentWith(t, nil, nil)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"status"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if !strings.Contains(out, "NAME") {
		t.Errorf("expected header even with no instances, got %q", out)
	}
}

// status <name> filters. Two plugins installed, one asked for: the other must
// NOT appear. A filter that returned everything would pass an assertion that
// only looked for the wanted row.
func TestStatus_FiltersByName(t *testing.T) {
	srv := agentWith(t, nil, []*weftv1.PluginInstance{
		instance("gitlab-runners-ha", "inst-1", "ci", "running", "vm-a"),
		instance("github-runners-ha", "inst-2", "ci", "degraded", "vm-b"),
	})
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"status", "gitlab-runners-ha"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if !strings.Contains(out, "inst-1") {
		t.Errorf("wanted instance missing: %q", out)
	}
	if strings.Contains(out, "inst-2") {
		t.Errorf("the filter let the other plugin through: %q", out)
	}
}

func TestUninstall_NoInstance(t *testing.T) {
	srv := agentWith(t, nil, nil)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"uninstall", "gitlab-runners-ha"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no installed instances") {
		t.Fatalf("expected no-installed-instances error, got %v", err)
	}
}

// Two instances of one plugin and no --instance: the CLI must refuse rather
// than pick one. Removing the wrong instance is not recoverable.
func TestUninstall_AmbiguousWithoutInstanceFlag(t *testing.T) {
	srv := agentWith(t, nil, []*weftv1.PluginInstance{
		instance("gitlab-runners-ha", "inst-1", "ci", "running"),
		instance("gitlab-runners-ha", "inst-2", "staging", "running"),
	})
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"uninstall", "gitlab-runners-ha"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "pass --instance") {
		t.Fatalf("expected a disambiguation error, got %v", err)
	}
}
