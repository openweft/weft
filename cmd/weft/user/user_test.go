package user

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "user" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "get", "me", "set-display-name", "rm"}
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
	srv.ListUsersFn = func(_ context.Context, _ *vzdv1.ListUsersRequest) (*vzdv1.ListUsersResponse, error) {
		return &vzdv1.ListUsersResponse{Users: []*vzdv1.UserInfo{
			{Uuid: "u1", OidcIssuer: "dex", OidcSubject: "s1", Email: "a@x", DisplayName: "Alice", Groups: []string{"g1"}, LastSeenAtUnixNs: 1700000000000000000},
			{Uuid: "u2"}, // empty fallback path
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "Alice") || !strings.Contains(out, "u2") {
		t.Errorf("missing rows: %q", out)
	}
}

func TestLs_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListUsersFn = func(_ context.Context, _ *vzdv1.ListUsersRequest) (*vzdv1.ListUsersResponse, error) {
		return &vzdv1.ListUsersResponse{Users: []*vzdv1.UserInfo{
			{Uuid: "u1", DisplayName: "Alice", LastSeenAtUnixNs: 1700000000000000000},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"display_name": "Alice"`) {
		t.Errorf("json missing display_name: %q", out)
	}
}

func TestLs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListUsersFn = func(_ context.Context, _ *vzdv1.ListUsersRequest) (*vzdv1.ListUsersResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── get ─────────────────────────────────────────────────────────────────────

func TestGet_Table(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetUserFn = func(_ context.Context, in *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error) {
		return &vzdv1.GetUserResponse{User: &vzdv1.UserInfo{Uuid: in.Uuid, DisplayName: "Bob"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"get", "u1"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "Bob") {
		t.Errorf("missing display: %q", out)
	}
}

func TestGet_JSON(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetUserFn = func(_ context.Context, in *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error) {
		return &vzdv1.GetUserResponse{User: &vzdv1.UserInfo{Uuid: in.Uuid, DisplayName: "Bob"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"get", "u1", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"display_name": "Bob"`) {
		t.Errorf("missing json: %q", out)
	}
}

func TestGet_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetUserFn = func(_ context.Context, _ *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"get", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── me ──────────────────────────────────────────────────────────────────────

func TestMe_Table(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.MeFn = func(_ context.Context, _ *vzdv1.MeRequest) (*vzdv1.MeResponse, error) {
		return &vzdv1.MeResponse{User: &vzdv1.UserInfo{Uuid: "u1", DisplayName: "Caller"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"me"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "Caller") {
		t.Errorf("missing: %q", out)
	}
}

func TestMe_JSON(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.MeFn = func(_ context.Context, _ *vzdv1.MeRequest) (*vzdv1.MeResponse, error) {
		return &vzdv1.MeResponse{User: &vzdv1.UserInfo{Uuid: "u1"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"me", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"uuid"`) {
		t.Errorf("missing json: %q", out)
	}
}

func TestMe_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.MeFn = func(_ context.Context, _ *vzdv1.MeRequest) (*vzdv1.MeResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"me"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── set-display-name ────────────────────────────────────────────────────────

func TestSetDisplayName_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetUserDisplayNameFn = func(_ context.Context, in *vzdv1.SetUserDisplayNameRequest) (*vzdv1.SetUserDisplayNameResponse, error) {
		return &vzdv1.SetUserDisplayNameResponse{User: &vzdv1.UserInfo{Uuid: in.Uuid, DisplayName: in.DisplayName}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-display-name", "u1", "NewName"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestSetDisplayName_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetUserDisplayNameFn = func(_ context.Context, _ *vzdv1.SetUserDisplayNameRequest) (*vzdv1.SetUserDisplayNameResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-display-name", "u1", "new"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── rm ──────────────────────────────────────────────────────────────────────

func TestRm_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "u1"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "u1") {
		t.Errorf("missing echo: %q", out)
	}
}

func TestRm_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteUserFn = func(_ context.Context, _ *vzdv1.DeleteUserRequest) (*vzdv1.DeleteUserResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── dial errors ─────────────────────────────────────────────────────────────

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"get", []string{"get", "u1"}},
		{"me", []string{"me"}},
		{"set-display-name", []string{"set-display-name", "u1", "n"}},
		{"rm", []string{"rm", "u1"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-user-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s: expected dial error", c.name)
			}
		})
	}
}
