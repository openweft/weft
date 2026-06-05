package bucket

import (
	"context"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "bucket" {
		t.Errorf("root.Use : got %q, want bucket", root.Use)
	}
	want := map[string]bool{
		"ls": false, "show <uuid>": false,
		"create": false, "rm <uuid>": false, "policy": false,
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
	srv.ListBucketsFn = func(_ context.Context, _ *weftv1.ListBucketsRequest) (*weftv1.ListBucketsResponse, error) {
		return &weftv1.ListBucketsResponse{Buckets: []*weftv1.BucketInfo{{Uuid: "u1", Name: "n1", Endpoint: "https://e"}}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ls: %v", err)
	}
}

func TestCreate_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateBucketFn = func(_ context.Context, req *weftv1.CreateBucketRequest) (*weftv1.CreateBucketResponse, error) {
		if req.Name != "b1" {
			t.Errorf("name: %q", req.Name)
		}
		return &weftv1.CreateBucketResponse{Bucket: &weftv1.BucketInfo{Uuid: "u1", Name: req.Name, Endpoint: req.Endpoint}, Created: true}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "--project", "p", "--name", "b1", "--endpoint", "https://e"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestPolicyGetSet(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetBucketPolicyFn = func(_ context.Context, _ *weftv1.GetBucketPolicyRequest) (*weftv1.GetBucketPolicyResponse, error) {
		return &weftv1.GetBucketPolicyResponse{Policy: `{"Version":"2012-10-17"}`}, nil
	}
	srv.SetBucketPolicyFn = func(_ context.Context, req *weftv1.SetBucketPolicyRequest) (*weftv1.SetBucketPolicyResponse, error) {
		if req.Policy == "" {
			t.Errorf("empty policy")
		}
		return &weftv1.SetBucketPolicyResponse{Bucket: &weftv1.BucketInfo{Uuid: req.Uuid, Policy: req.Policy}}, nil
	}
	get := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	get.SetArgs([]string{"policy", "get", "u1"})
	if err := get.Execute(); err != nil {
		t.Fatalf("policy get: %v", err)
	}
	set := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	set.SetArgs([]string{"policy", "set", "u1", `{"Version":"2012-10-17"}`})
	if err := set.Execute(); err != nil {
		t.Fatalf("policy set: %v", err)
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"create", []string{"create", "--project", "p", "--name", "n", "--endpoint", "e"}},
		{"rm", []string{"rm", "u1"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-bk-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error", c.name)
			}
		})
	}
}
