package script

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
	vzdv1 "github.com/openweft/weft-proto"
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

// ── structure ──────────────────────────────────────────────────────────────

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "script" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "get", "set", "rm"}
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		got[strings.SplitN(c.Use, " ", 2)[0]] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

// ── ls ──────────────────────────────────────────────────────────────────────

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListScriptsFn = func(_ context.Context, _ *vzdv1.ListScriptsRequest) (*vzdv1.ListScriptsResponse, error) {
		return &vzdv1.ListScriptsResponse{Scripts: []*vzdv1.Script{
			{Name: "deploy", Description: "Bring up the static site", Body: "echo hi\necho there\n", UpdatedAt: "2026-05-29T10:00:00Z", UpdatedBy: "alice@x"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	for _, want := range []string{"deploy", "alice@x", "Bring up the static site"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q in :\n%s", want, out)
		}
	}
}

// ── get ─────────────────────────────────────────────────────────────────────

func TestGet_BodyOnly(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetScriptFn = func(_ context.Context, in *vzdv1.GetScriptRequest) (*vzdv1.GetScriptResponse, error) {
		return &vzdv1.GetScriptResponse{Script: &vzdv1.Script{Name: in.Name, Body: "#!/bin/sh\necho ok\n"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"get", "deploy", "--body-only"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if out != "#!/bin/sh\necho ok\n" {
		t.Errorf("--body-only should write raw body, got %q", out)
	}
}

func TestGet_FullRender(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetScriptFn = func(_ context.Context, in *vzdv1.GetScriptRequest) (*vzdv1.GetScriptResponse, error) {
		return &vzdv1.GetScriptResponse{Script: &vzdv1.Script{
			Name: in.Name, Description: "demo", Body: "echo hi\n",
			UpdatedAt: "2026-05-29T10:00:00Z", UpdatedBy: "alice@x",
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"get", "deploy"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	for _, want := range []string{"# name        : deploy", "# updated_by  : alice@x", "echo hi"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in :\n%s", want, out)
		}
	}
}

// ── set ─────────────────────────────────────────────────────────────────────

func TestSet_FromFile(t *testing.T) {
	srv := testutil.NewServer(t)
	var saved *vzdv1.Script
	srv.SetScriptFn = func(_ context.Context, in *vzdv1.SetScriptRequest) (*vzdv1.SetScriptResponse, error) {
		saved = in.Script
		return &vzdv1.SetScriptResponse{Script: in.Script}, nil
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set", "deploy", "--file", path, "--description", "demo"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if saved == nil || saved.Name != "deploy" || saved.Description != "demo" || !strings.Contains(saved.Body, "from-file") {
		t.Errorf("server saw %+v, fields didn't all transit", saved)
	}
	if !strings.Contains(out, "set\tdeploy") {
		t.Errorf("expected 'set\\tdeploy' in :\n%s", out)
	}
}

func TestSet_FromBodyInline(t *testing.T) {
	srv := testutil.NewServer(t)
	var saved *vzdv1.Script
	srv.SetScriptFn = func(_ context.Context, in *vzdv1.SetScriptRequest) (*vzdv1.SetScriptResponse, error) {
		saved = in.Script
		return &vzdv1.SetScriptResponse{Script: in.Script}, nil
	}
	captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set", "inline", "--body", "echo inline"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if saved == nil || saved.Body != "echo inline" {
		t.Errorf("inline body didn't transit : %+v", saved)
	}
}

func TestSet_FileAndBodyMutuallyExclusive(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set", "x", "--file", "a", "--body", "b"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error, got %v", err)
	}
}

func TestSet_RequiresFileOrBody(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set", "x"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "one of --file or --body") {
		t.Errorf("expected one-of error, got %v", err)
	}
}

func TestSet_ErrorBubblesUp(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetScriptFn = func(_ context.Context, _ *vzdv1.SetScriptRequest) (*vzdv1.SetScriptResponse, error) {
		return nil, errors.New("name is required")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set", "x", "--body", "echo"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected server error, got %v", err)
	}
}

// ── rm ──────────────────────────────────────────────────────────────────────

func TestRm_PassesName(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen string
	srv.DeleteScriptFn = func(_ context.Context, in *vzdv1.DeleteScriptRequest) (*vzdv1.DeleteScriptResponse, error) {
		seen = in.Name
		return &vzdv1.DeleteScriptResponse{Deleted: in.Name}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "deploy"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen != "deploy" {
		t.Errorf("server got name=%q, want deploy", seen)
	}
	if !strings.Contains(out, "deleted\tdeploy") {
		t.Errorf("expected 'deleted\\tdeploy' in :\n%s", out)
	}
}
