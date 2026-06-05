package registry

import (
	"context"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "registry" {
		t.Errorf("root.Use : got %q", root.Use)
	}
	want := map[string]bool{
		"ls":                       false,
		"set":                      false,
		"rm <uuid|name>":           false,
		"search <uuid|name> <query>": false,
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
	srv.ListRegistryRemotesFn = func(_ context.Context, _ *weftv1.ListRegistryRemotesRequest) (*weftv1.ListRegistryRemotesResponse, error) {
		return &weftv1.ListRegistryRemotesResponse{Remotes: []*weftv1.RegistryRemoteInfo{{Uuid: "u1", Name: "ghcr", Endpoint: "ghcr.io"}}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ls: %v", err)
	}
}

func TestSet_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetRegistryRemoteFn = func(_ context.Context, req *weftv1.SetRegistryRemoteRequest) (*weftv1.SetRegistryRemoteResponse, error) {
		if req.Name != "ghcr" || req.Endpoint != "ghcr.io" {
			t.Errorf("req: %+v", req)
		}
		return &weftv1.SetRegistryRemoteResponse{Remote: &weftv1.RegistryRemoteInfo{Uuid: "u1", Name: req.Name, Endpoint: req.Endpoint}, Created: true}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set", "--name", "ghcr", "--endpoint", "ghcr.io"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set: %v", err)
	}
}

func TestRm_ByName(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteRegistryRemoteFn = func(_ context.Context, req *weftv1.DeleteRegistryRemoteRequest) (*weftv1.DeleteRegistryRemoteResponse, error) {
		if req.Name != "ghcr" {
			t.Errorf("name: %q", req.Name)
		}
		return &weftv1.DeleteRegistryRemoteResponse{DeletedUuid: "u1"}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "ghcr"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rm: %v", err)
	}
}

func TestSearch_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SearchRegistryRemoteFn = func(_ context.Context, req *weftv1.SearchRegistryRemoteRequest) (*weftv1.SearchRegistryRemoteResponse, error) {
		if req.Name != "ghcr" || req.Query != "weft" {
			t.Errorf("req: %+v", req)
		}
		return &weftv1.SearchRegistryRemoteResponse{Repositories: []string{"openweft/weft"}, RegistryName: req.Name}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"search", "ghcr", "weft"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("search: %v", err)
	}
}

func TestLooksLikeUUID(t *testing.T) {
	if !looksLikeUUID("e9c3b6c4-9ea2-4f3a-9c1d-2d5a2e3d6b7c") {
		t.Error("expected UUID")
	}
	if looksLikeUUID("ghcr") {
		t.Error("expected non-UUID")
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"set", []string{"set", "--name", "n", "--endpoint", "e"}},
		{"rm", []string{"rm", "n"}},
		{"search", []string{"search", "n", "q"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-rg-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error", c.name)
			}
		})
	}
}
