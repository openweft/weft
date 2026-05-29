package project

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

// ── structure ───────────────────────────────────────────────────────────────

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "project" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "create", "rename", "rm", "add-user", "remove-user", "members"}
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

// ── helpers ─────────────────────────────────────────────────────────────────

func TestLooksLikeUUID(t *testing.T) {
	if !looksLikeUUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Error("valid UUID rejected")
	}
	if looksLikeUUID("aaaaaaaabbbbccccddddeeeeeeeeeeeexxxxx") {
		t.Error("wrong length should be rejected")
	}
	if looksLikeUUID("aaaaaaaa-bbbb-cccc-dddd-zzzzzzzzzzzz") {
		t.Error("non-hex rune accepted")
	}
	if looksLikeUUID("aaaaaaaaXbbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Error("missing hyphen accepted")
	}
}

// ── ls ──────────────────────────────────────────────────────────────────────

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return &vzdv1.ListProjectsResponse{Projects: []*vzdv1.ProjectInfo{
			{Uuid: "u1", Name: "p1", CreatedAtUnixNs: 1700000000000000000},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "p1") {
		t.Errorf("missing project: %q", out)
	}
}

func TestLs_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return &vzdv1.ListProjectsResponse{Projects: []*vzdv1.ProjectInfo{
			{Uuid: "u1", Name: "p1"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"uuid": "u1"`) {
		t.Errorf("missing json uuid: %q", out)
	}
}

func TestLs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── create ──────────────────────────────────────────────────────────────────

func TestCreate_NewProject(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateProjectFn = func(_ context.Context, in *vzdv1.CreateProjectRequest) (*vzdv1.CreateProjectResponse, error) {
		return &vzdv1.CreateProjectResponse{Project: &vzdv1.ProjectInfo{Uuid: "u1", Name: in.Name}, Created: true}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"create", "p"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "created") {
		t.Errorf("expected created tag: %q", out)
	}
}

func TestCreate_ExistingProject(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateProjectFn = func(_ context.Context, in *vzdv1.CreateProjectRequest) (*vzdv1.CreateProjectResponse, error) {
		return &vzdv1.CreateProjectResponse{Project: &vzdv1.ProjectInfo{Uuid: "u1", Name: in.Name}, Created: false}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"create", "p"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "exists") {
		t.Errorf("expected exists tag: %q", out)
	}
}

func TestCreate_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateProjectFn = func(_ context.Context, _ *vzdv1.CreateProjectRequest) (*vzdv1.CreateProjectResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "p"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── rename ──────────────────────────────────────────────────────────────────

func TestRename_ByUUID(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenameProjectFn = func(_ context.Context, in *vzdv1.RenameProjectRequest) (*vzdv1.RenameProjectResponse, error) {
		return &vzdv1.RenameProjectResponse{Project: &vzdv1.ProjectInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "new"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestRename_ByName_LookupSuccess(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return &vzdv1.ListProjectsResponse{Projects: []*vzdv1.ProjectInfo{
			{Uuid: "u1", Name: "alpha"},
		}}, nil
	}
	srv.RenameProjectFn = func(_ context.Context, in *vzdv1.RenameProjectRequest) (*vzdv1.RenameProjectResponse, error) {
		if in.Uuid != "u1" {
			t.Errorf("expected uuid=u1, got %q", in.Uuid)
		}
		return &vzdv1.RenameProjectResponse{Project: &vzdv1.ProjectInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "alpha", "beta"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestRename_ByName_NotFound(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return &vzdv1.ListProjectsResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "ghost", "new"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no project named") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestRename_ListProjectsError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "alpha", "beta"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected ListProjects error")
	}
}

func TestRename_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenameProjectFn = func(_ context.Context, _ *vzdv1.RenameProjectRequest) (*vzdv1.RenameProjectResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "new"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── rm ──────────────────────────────────────────────────────────────────────

func TestRm_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "aaaaaaaa") {
		t.Errorf("missing uuid echo: %q", out)
	}
}

func TestRm_ResolveError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "name-only"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

func TestRm_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteProjectFn = func(_ context.Context, _ *vzdv1.DeleteProjectRequest) (*vzdv1.DeleteProjectResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── add-user ────────────────────────────────────────────────────────────────

func TestAddUser_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.AddProjectMemberFn = func(_ context.Context, in *vzdv1.AddProjectMemberRequest) (*vzdv1.AddProjectMemberResponse, error) {
		return &vzdv1.AddProjectMemberResponse{UserUuids: []string{in.UserUuid}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"add-user", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "u-1"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "added") || !strings.Contains(out, "members=1") {
		t.Errorf("expected added banner: %q", out)
	}
}

func TestAddUser_ResolveError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"add-user", "alpha", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected resolve error")
	}
}

func TestAddUser_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.AddProjectMemberFn = func(_ context.Context, _ *vzdv1.AddProjectMemberRequest) (*vzdv1.AddProjectMemberResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"add-user", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected rpc error")
	}
}

// ── remove-user ─────────────────────────────────────────────────────────────

func TestRemoveUser_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RemoveProjectMemberFn = func(_ context.Context, _ *vzdv1.RemoveProjectMemberRequest) (*vzdv1.RemoveProjectMemberResponse, error) {
		return &vzdv1.RemoveProjectMemberResponse{UserUuids: []string{}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"remove-user", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "u-1"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "removed") {
		t.Errorf("expected removed banner: %q", out)
	}
}

func TestRemoveUser_ResolveError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"remove-user", "alpha", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected resolve error")
	}
}

func TestRemoveUser_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RemoveProjectMemberFn = func(_ context.Context, _ *vzdv1.RemoveProjectMemberRequest) (*vzdv1.RemoveProjectMemberResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"remove-user", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected rpc error")
	}
}

// ── members ─────────────────────────────────────────────────────────────────

func TestMembers_EmptyList(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectMembersFn = func(_ context.Context, _ *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error) {
		return &vzdv1.ListProjectMembersResponse{}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"members", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "(no platform-managed members)") {
		t.Errorf("expected empty hint: %q", out)
	}
}

func TestMembers_RawUUIDList(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectMembersFn = func(_ context.Context, _ *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error) {
		return &vzdv1.ListProjectMembersResponse{UserUuids: []string{"u-1", "u-2"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"members", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "u-1") || !strings.Contains(out, "u-2") {
		t.Errorf("missing uuids: %q", out)
	}
}

func TestMembers_ResolveMode(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectMembersFn = func(_ context.Context, _ *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error) {
		return &vzdv1.ListProjectMembersResponse{UserUuids: []string{"u-good", "u-bad"}}, nil
	}
	srv.GetUserFn = func(_ context.Context, in *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error) {
		if in.Uuid == "u-bad" {
			return nil, errors.New("not found")
		}
		return &vzdv1.GetUserResponse{User: &vzdv1.UserInfo{Uuid: in.Uuid, DisplayName: "Alice", Email: "a@x"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"members", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "--resolve"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "Alice") {
		t.Errorf("missing display name: %q", out)
	}
	// u-bad row should still render as dashes.
	if !strings.Contains(out, "u-bad") {
		t.Errorf("missing u-bad row: %q", out)
	}
}

func TestMembers_ResolveMode_PartialUser(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectMembersFn = func(_ context.Context, _ *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error) {
		return &vzdv1.ListProjectMembersResponse{UserUuids: []string{"u-1"}}, nil
	}
	// User exists but DisplayName + Email are empty → fallthrough
	// branch in resolve mode (`name` and `email` stay as "-").
	srv.GetUserFn = func(_ context.Context, in *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error) {
		return &vzdv1.GetUserResponse{User: &vzdv1.UserInfo{Uuid: in.Uuid}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"members", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "--resolve"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "u-1") {
		t.Errorf("missing uuid: %q", out)
	}
}

func TestMembers_ResolveError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectsFn = func(_ context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"members", "alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected resolve error")
	}
}

func TestMembers_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListProjectMembersFn = func(_ context.Context, _ *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"members", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── dial errors across all subcommands ──────────────────────────────────────

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"create", []string{"create", "p"}},
		{"rename", []string{"rename", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "x"}},
		{"rm", []string{"rm", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}},
		{"add-user", []string{"add-user", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "u"}},
		{"remove-user", []string{"remove-user", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "u"}},
		{"members", []string{"members", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-proj-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s: expected dial error", c.name)
			}
		})
	}
}
