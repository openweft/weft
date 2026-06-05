package share

import (
	"context"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "share" {
		t.Errorf("root.Use : got %q, want share", root.Use)
	}
	want := map[string]bool{
		"ls": false, "show <uuid>": false,
		"create": false, "rm <uuid>": false,
		"attach": false, "detach": false,
	}
	for _, sub := range root.Commands() {
		want[sub.Use] = true
	}
	for use, seen := range want {
		if !seen {
			t.Errorf("missing subcommand %q", use)
		}
	}
}

func TestLs_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListSharesFn = func(_ context.Context, req *weftv1.ListSharesRequest) (*weftv1.ListSharesResponse, error) {
		if req.Project != "proj1" {
			t.Errorf("project: got %q", req.Project)
		}
		return &weftv1.ListSharesResponse{Shares: []*weftv1.ShareInfo{{Uuid: "u1", Name: "n1", SizeGb: 10, Status: "active"}}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls", "--project", "proj1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ls: %v", err)
	}
}

func TestCreate_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateShareFn = func(_ context.Context, req *weftv1.CreateShareRequest) (*weftv1.CreateShareResponse, error) {
		if req.Name != "share1" || req.SizeGb != 50 || !req.Readonly {
			t.Errorf("create req: %+v", req)
		}
		return &weftv1.CreateShareResponse{Share: &weftv1.ShareInfo{
			Uuid: "u1", Name: req.Name, SizeGb: req.SizeGb, Backend: "cubefs", Status: "provisioning",
		}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "--project", "proj1", "--name", "share1", "--size-gb", "50", "--readonly"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestRm_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteShareFn = func(_ context.Context, req *weftv1.DeleteShareRequest) (*weftv1.DeleteShareResponse, error) {
		if req.Uuid != "u1" {
			t.Errorf("uuid: %q", req.Uuid)
		}
		return &weftv1.DeleteShareResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "u1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rm: %v", err)
	}
}

func TestShow_Match(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListSharesFn = func(_ context.Context, _ *weftv1.ListSharesRequest) (*weftv1.ListSharesResponse, error) {
		return &weftv1.ListSharesResponse{Shares: []*weftv1.ShareInfo{
			{Uuid: "u1", Name: "n1"},
			{Uuid: "u2", Name: "n2"},
		}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"show", "u2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("show: %v", err)
	}
}

func TestShow_Miss(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListSharesFn = func(_ context.Context, _ *weftv1.ListSharesRequest) (*weftv1.ListSharesResponse, error) {
		return &weftv1.ListSharesResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"show", "absent"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected miss error")
	}
}

func TestAttach_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.PublishShareToProjectFn = func(_ context.Context, req *weftv1.PublishShareToProjectRequest) (*weftv1.PublishShareToProjectResponse, error) {
		if req.Mount == nil || req.Mount.Id == "" {
			t.Errorf("mount missing")
		}
		return &weftv1.PublishShareToProjectResponse{VmCount: 3}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{
		"attach",
		"--project", "p", "--id", "m1", "--mount-point", "/data",
		"--volume", "vol", "--owner", "ow", "--masters", "h:1",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("attach: %v", err)
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"create", []string{"create", "--project", "p", "--name", "n", "--size-gb", "1"}},
		{"rm", []string{"rm", "u1"}},
		{"show", []string{"show", "u1"}},
		{"detach", []string{"detach", "--project", "p", "--id", "m", "--mount-point", "/d"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-share-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error", c.name)
			}
		})
	}
}
