package volumeproperty

import (
	"context"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "property" {
		t.Errorf("root.Use : got %q, want property", root.Use)
	}
	want := map[string]bool{
		"set <volume-uuid> <key> <value>": false,
		"get <volume-uuid> <key>":         false,
		"delete <volume-uuid> <key>":      false,
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

func TestSet_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetVolumePropertyFn = func(_ context.Context, req *weftv1.SetVolumePropertyRequest) (*weftv1.SetVolumePropertyResponse, error) {
		if req.VolumeUuid != "v1" || req.Key != "k" || req.Value != "v" {
			t.Errorf("req: %+v", req)
		}
		return &weftv1.SetVolumePropertyResponse{Property: &weftv1.VolumePropertyInfo{VolumeUuid: req.VolumeUuid, Key: req.Key, Value: req.Value}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set", "v1", "k", "v"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set: %v", err)
	}
}

func TestGet_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetVolumePropertyFn = func(_ context.Context, _ *weftv1.GetVolumePropertyRequest) (*weftv1.GetVolumePropertyResponse, error) {
		return &weftv1.GetVolumePropertyResponse{Property: &weftv1.VolumePropertyInfo{Value: "v"}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"get", "v1", "k"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get: %v", err)
	}
}

func TestDelete_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteVolumePropertyFn = func(_ context.Context, _ *weftv1.DeleteVolumePropertyRequest) (*weftv1.DeleteVolumePropertyResponse, error) {
		return &weftv1.DeleteVolumePropertyResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"delete", "v1", "k"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"set", []string{"set", "v", "k", "x"}},
		{"get", []string{"get", "v", "k"}},
		{"delete", []string{"delete", "v", "k"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-vp-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error", c.name)
			}
		})
	}
}
