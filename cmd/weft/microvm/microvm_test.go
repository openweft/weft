package microvm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written. Mirrors the instance/host sub-package tests.
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

// --- Command structure ---

func TestCommand_StructureHasAllSubcommands(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "microvm" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"run", "pull", "ls", "rm", "logs", "init-build"}
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		got[strings.SplitN(c.Use, " ", 2)[0]] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing subcommand %q (have %v)", w, got)
		}
	}
}

// --- ls ---

func TestLs_FiltersMicroVMs(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMsFn = func(_ context.Context, _ *weftv1.ListVMsRequest) (*weftv1.ListVMsResponse, error) {
		return &weftv1.ListVMsResponse{Vms: []*weftv1.VMInfo{
			{Name: "weft-microvm-alpine_3.21", State: weftv1.VMState_VM_STATE_RUNNING},
			{Name: "classic-vm", State: weftv1.VMState_VM_STATE_RUNNING},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "weft-microvm-alpine_3.21") {
		t.Errorf("microVM missing: %q", out)
	}
	if strings.Contains(out, "classic-vm") {
		t.Errorf("classic VM should be filtered out: %q", out)
	}
}

func TestLs_AllIncludesNonMicroVMs(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMsFn = func(_ context.Context, _ *weftv1.ListVMsRequest) (*weftv1.ListVMsResponse, error) {
		return &weftv1.ListVMsResponse{Vms: []*weftv1.VMInfo{
			{Name: "weft-microvm-a", State: weftv1.VMState_VM_STATE_RUNNING},
			{Name: "classic-vm", State: weftv1.VMState_VM_STATE_RUNNING},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "-a"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "classic-vm") {
		t.Errorf("--all should include classic VM: %q", out)
	}
}

func TestLs_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMsFn = func(_ context.Context, in *weftv1.ListVMsRequest) (*weftv1.ListVMsResponse, error) {
		if in.Project != "team-net" {
			t.Errorf("project flag not threaded: %q", in.Project)
		}
		return &weftv1.ListVMsResponse{Vms: []*weftv1.VMInfo{
			{Name: "weft-microvm-x", State: weftv1.VMState_VM_STATE_RUNNING, Ip: "10.0.0.5"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--format", "json", "--project", "team-net"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name":"weft-microvm-x"`) {
		t.Errorf("json missing: %q", out)
	}
}

func TestLs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMsFn = func(_ context.Context, _ *weftv1.ListVMsRequest) (*weftv1.ListVMsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected RPC error")
	}
}

func TestLs_DialError(t *testing.T) {
	cmd := Command(strPtr("/tmp/no-such-microvm-ls.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

// --- rm ---

func TestRm_StopThenDelete(t *testing.T) {
	srv := testutil.NewServer(t)
	var stopped, deleted string
	srv.StopVMFn = func(_ context.Context, in *weftv1.StopVMRequest) (*weftv1.StopVMResponse, error) {
		stopped = in.Name
		return &weftv1.StopVMResponse{}, nil
	}
	srv.DeleteVMFn = func(_ context.Context, in *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
		deleted = in.Name
		return &weftv1.DeleteVMResponse{}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		// Bare image ref → resolveName prefixes & sanitises it.
		cmd.SetArgs([]string{"rm", "alpine:3.21"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if stopped != "weft-microvm-alpine_3.21" || deleted != "weft-microvm-alpine_3.21" {
		t.Errorf("stop=%q delete=%q", stopped, deleted)
	}
	if !strings.Contains(out, "weft-microvm-alpine_3.21") {
		t.Errorf("rm should echo the removed name: %q", out)
	}
}

func TestRm_AlreadyPrefixedName(t *testing.T) {
	srv := testutil.NewServer(t)
	var deleted string
	srv.DeleteVMFn = func(_ context.Context, in *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
		deleted = in.Name
		return &weftv1.DeleteVMResponse{}, nil
	}
	_ = captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "weft-microvm-already"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if deleted != "weft-microvm-already" {
		t.Errorf("prefixed name should pass through: %q", deleted)
	}
}

func TestRm_ForceSkipsStop(t *testing.T) {
	srv := testutil.NewServer(t)
	stopCalled := false
	srv.StopVMFn = func(_ context.Context, _ *weftv1.StopVMRequest) (*weftv1.StopVMResponse, error) {
		stopCalled = true
		return &weftv1.StopVMResponse{}, nil
	}
	_ = captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "-f", "weft-microvm-x"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if stopCalled {
		t.Error("--force should skip StopVM")
	}
}

func TestRm_StopAlreadyStopped_Tolerated(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.StopVMFn = func(_ context.Context, _ *weftv1.StopVMRequest) (*weftv1.StopVMResponse, error) {
		return nil, errors.New("vm is not running")
	}
	deleted := false
	srv.DeleteVMFn = func(_ context.Context, _ *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
		deleted = true
		return &weftv1.DeleteVMResponse{}, nil
	}
	_ = captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "weft-microvm-x"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("an 'already stopped' StopVM error should be tolerated: %v", err)
		}
	})
	if !deleted {
		t.Error("DeleteVM should still run after a tolerated stop error")
	}
}

func TestRm_StopRealError_Reported(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.StopVMFn = func(_ context.Context, _ *weftv1.StopVMRequest) (*weftv1.StopVMResponse, error) {
		return nil, errors.New("disk on fire")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "weft-microvm-x"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("a non-'already-stopped' stop error should surface")
	}
}

func TestRm_DeleteError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteVMFn = func(_ context.Context, _ *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
		return nil, errors.New("delete failed")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "-f", "weft-microvm-x"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected delete error")
	}
}

func TestRm_MultipleArgs_FirstErrorReturned(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteVMFn = func(_ context.Context, in *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
		if in.Name == "weft-microvm-bad" {
			return nil, errors.New("nope")
		}
		return &weftv1.DeleteVMResponse{}, nil
	}
	_ = captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "-f", "weft-microvm-bad", "weft-microvm-good"})
		if err := cmd.Execute(); err == nil {
			t.Error("expected the first error to be returned even though the second succeeds")
		}
	})
}

func TestRm_DialError(t *testing.T) {
	cmd := Command(strPtr("/tmp/no-such-microvm-rm.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "weft-microvm-x"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestRm_RequiresArg(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("rm with no args should error (MinimumNArgs 1)")
	}
}

// --- logs ---

func TestLogs_ContainerOutputFiltered(t *testing.T) {
	srv := testutil.NewServer(t)
	console := "kernel boot junk\n" +
		"weft-microvm-init: starting\n" +
		"WEFT_MARK exec_ready\n" +
		"hello from container\n" +
		"weft-microvm-init: noise inside window\n" +
		"WEFT_MARK child_exited\n" +
		"trailing kernel\n"
	srv.VMLogsFn = func(_ context.Context, in *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		if in.Name != "weft-microvm-app" {
			t.Errorf("name not resolved: %q", in.Name)
		}
		return &weftv1.VMLogsResponse{Contents: []byte(console)}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"logs", "weft-microvm-app"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "hello from container") {
		t.Errorf("container output missing: %q", out)
	}
	if strings.Contains(out, "kernel boot junk") || strings.Contains(out, "noise inside window") ||
		strings.Contains(out, "WEFT_MARK") {
		t.Errorf("filter leaked non-container lines: %q", out)
	}
}

func TestLogs_Raw(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMLogsFn = func(_ context.Context, in *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		if in.TailBytes != 128 {
			t.Errorf("tail flag not threaded: %d", in.TailBytes)
		}
		return &weftv1.VMLogsResponse{Contents: []byte("kernel junk\nWEFT_MARK exec_ready\n")}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"logs", "--raw", "--tail", "128", "weft-microvm-app"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "kernel junk") || !strings.Contains(out, "WEFT_MARK") {
		t.Errorf("--raw should dump everything: %q", out)
	}
}

func TestLogs_EmptyContents(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMLogsFn = func(_ context.Context, _ *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		return &weftv1.VMLogsResponse{Contents: nil}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"logs", "weft-microvm-app"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if out != "" {
		t.Errorf("empty contents should produce no output, got %q", out)
	}
}

func TestLogs_NoExecReady_EmptyFiltered(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMLogsFn = func(_ context.Context, _ *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		// No exec_ready marker → containerOutput returns nil, so the
		// command writes nothing.
		return &weftv1.VMLogsResponse{Contents: []byte("just kernel\nweft-microvm-init: boot\n")}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"logs", "weft-microvm-app"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if out != "" {
		t.Errorf("no exec_ready should yield empty container output, got %q", out)
	}
}

func TestLogs_TrailingNewlineAdded(t *testing.T) {
	srv := testutil.NewServer(t)
	// Raw output without a trailing newline → the command appends one.
	srv.VMLogsFn = func(_ context.Context, _ *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		return &weftv1.VMLogsResponse{Contents: []byte("no newline at end")}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"logs", "--raw", "weft-microvm-app"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("expected a trailing newline to be added, got %q", out)
	}
}

func TestLogs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMLogsFn = func(_ context.Context, _ *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"logs", "weft-microvm-app"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected RPC error")
	}
}

func TestLogs_DialError(t *testing.T) {
	cmd := Command(strPtr("/tmp/no-such-microvm-logs.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"logs", "weft-microvm-app"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestLogs_RequiresExactlyOneArg(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"logs"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("logs with no args should error (ExactArgs 1)")
	}
}

// --- containerOutput unit-level edge cases ---

func TestContainerOutput_EdgeCases(t *testing.T) {
	t.Run("no exec_ready", func(t *testing.T) {
		if got := containerOutput([]byte("boot only")); got != nil {
			t.Errorf("got %q", got)
		}
	})
	t.Run("exec_ready as last line (no newline after)", func(t *testing.T) {
		// startMark present but no newline after it → returns nil.
		if got := containerOutput([]byte("WEFT_MARK exec_ready")); got != nil {
			t.Errorf("got %q", got)
		}
	})
	t.Run("no child_exited runs to end", func(t *testing.T) {
		in := "WEFT_MARK exec_ready\noutput line\n"
		got := string(containerOutput([]byte(in)))
		if got != "output line\n" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("drops WEFT_MARK lines inside the window", func(t *testing.T) {
		in := "WEFT_MARK exec_ready\nreal output\nWEFT_MARK some_inner_marker\nmore output\n"
		got := string(containerOutput([]byte(in)))
		if got != "real output\nmore output\n" {
			t.Errorf("inner WEFT_MARK not dropped: %q", got)
		}
	})
}

func TestIsAlreadyStopped(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("vm is not running"), true},
		{errors.New("already stopped"), true},
		{errors.New("state=STOPPED"), true},
		{errors.New("disk on fire"), false},
	}
	for _, c := range cases {
		if got := isAlreadyStopped(c.err); got != c.want {
			t.Errorf("isAlreadyStopped(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// --- run (delegates to lib microvm.Run) ---

func TestRun_RequiresArg(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"run"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("run with no args should error (MinimumNArgs 1)")
	}
}

func TestRun_NoAutoPull_ErrorPath(t *testing.T) {
	// run delegates to microvm.Run; with WEFT_NO_AUTO_PULL=1 and an
	// unpulled image the lib returns an actionable error. This exercises
	// the cobra front-end's Args construction + RunE without needing a
	// real boot/daemon.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WEFT_NO_AUTO_PULL", "1")
	cmd := Command(strPtr("/tmp/unused-run.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"run", "definitely-not-pulled:0.0.0"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "pull") {
		t.Fatalf("expected pull-related error, got %v", err)
	}
}

func TestRun_CmdOverrideAfterDash(t *testing.T) {
	// With a "--" tail, the override command replaces args; the missing
	// boot artefacts make the lib error after Args is built — which is
	// enough to confirm the front-end forwards the override without a
	// real daemon. We seed a pulled rootfs so the override-rewrite path
	// runs, then fail at boot-artefact resolution.
	//
	// Two defences against accidentally hitting a real registry :
	//
	//   * WEFT_NO_AUTO_PULL=1 — strict offline ; the lib hard-errors
	//     before auto-pull instead of dialling Docker Hub.
	//   * Sentinel image name — even if the offline gate were
	//     bypassed, "weft-test-fixture:override" doesn't resolve
	//     anywhere on the public registries.
	//
	// The cache path is the lib's real layout
	// ($XDG_DATA_HOME/weft-microvm/images/<refsafe>/rootfs/.weft-microvm/) ;
	// the legacy weft-microvm/images/ path is gone since the rename to
	// weft-microvm.
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("WEFT_KERNEL", "")
	t.Setenv("WEFT_INITRD", "")
	t.Setenv("WEFT_INIT_ISO", "")
	t.Setenv("WEFT_NO_AUTO_PULL", "1")
	const image = "weft-test-fixture:override"
	const refsafe = "weft-test-fixture_override"
	rootfs := filepath.Join(xdg, "weft-microvm", "images", refsafe, "rootfs", ".weft-microvm")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "config.json"),
		[]byte(`{"process":{"args":["/orig"],"env":["X=1"],"cwd":"/"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := Command(strPtr("/tmp/unused-run2.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"run", image, "--", "sh", "-c", "echo hi"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	// No kernel/iso seeded → boot-artefact resolution fails, proving we
	// got past Args construction + the override rewrite.
	if err == nil || !strings.Contains(err.Error(), "boot artefacts") {
		t.Fatalf("expected boot-artefact error, got %v", err)
	}
	// Confirm the override was written to config.json.
	b, _ := os.ReadFile(filepath.Join(rootfs, "config.json"))
	if !strings.Contains(string(b), "echo hi") {
		t.Errorf("command override not applied to config: %s", b)
	}
}

// --- pull (delegates to lib microvm.Pull) ---

func TestPull_RequiresExactlyOneArg(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"pull"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("pull with no args should error (ExactArgs 1)")
	}
}

func TestPull_DelegatesToLib_ErrorSurfaces(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// Point at a localhost port with nothing listening → the lib's Pull
	// fails fast (offline). Confirms the front-end forwards the ref.
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"pull", "127.0.0.1:1/no/such:tag"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected pull error against a dead registry")
	}
}

// --- init-build ---

// minimalELF is just enough of an ELF magic header to look like a Linux
// binary; initbuild.Pack copies raw bytes, so any content works.
var minimalELF = []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}

func TestInitBuild_HappyPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "weft-microvm-init")
	if err := os.WriteFile(bin, minimalELF, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "initrd.cpio.gz")
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"init-build", bin, "-o", out})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("output archive missing or empty: err=%v", err)
	}
}

func TestInitBuild_DefaultOutputPath(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	dir := t.TempDir()
	bin := filepath.Join(dir, "weft-microvm-init")
	if err := os.WriteFile(bin, minimalELF, 0o755); err != nil {
		t.Fatal(err)
	}
	// defaultInitrdPath() returns $XDG_DATA_HOME/weft-microvm/initrd; the parent
	// dir is created by a prior `pull`/cache step in normal use, so the
	// test pre-creates it (PackToFile only opens the file, it does not
	// MkdirAll the parent).
	if err := os.MkdirAll(filepath.Join(xdg, "weft-microvm"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No -o flag → defaultInitrdPath() under XDG_DATA_HOME/weft-microvm/initrd.
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"init-build", bin})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(xdg, "weft-microvm", "initrd")); err != nil {
		t.Fatalf("default output not created: %v", err)
	}
}

func TestInitBuild_MissingSource(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"init-build", "/no/such/binary", "-o", filepath.Join(t.TempDir(), "out")})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing source binary")
	}
}

func TestInitBuild_RequiresArg(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"init-build"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("init-build with no args should error (ExactArgs 1)")
	}
}

func TestDefaultInitrdPath_Fallbacks(t *testing.T) {
	t.Run("XDG set", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/xdg")
		if got := defaultInitrdPath(); got != filepath.Join("/xdg", "weft-microvm", "initrd") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/home/tester")
		got := defaultInitrdPath()
		if !strings.HasSuffix(got, filepath.Join(".local", "share", "weft-microvm", "initrd")) {
			t.Errorf("got %q", got)
		}
	})
	t.Run("/tmp fallback", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "")
		if got := defaultInitrdPath(); got != "/tmp/weft-microvm-initrd" {
			t.Errorf("got %q", got)
		}
	})
}
