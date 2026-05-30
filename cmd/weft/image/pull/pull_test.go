package pull

import (
	"context"
	"errors"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "pull" {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestPull_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-pull.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestPull_DefaultFlags(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.PullImagesRequest
	srv.PullImagesFn = func(_ context.Context, in *weftv1.PullImagesRequest) (*weftv1.PullImagesResponse, error) {
		got = in
		return &weftv1.PullImagesResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if got.ConfigDir != ".mock/hcl" {
		t.Errorf("default configDir = %q", got.ConfigDir)
	}
	if got.Parallel != 4 {
		t.Errorf("default parallel = %d", got.Parallel)
	}
}

func TestPull_CustomFlags(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.PullImagesRequest
	srv.PullImagesFn = func(_ context.Context, in *weftv1.PullImagesRequest) (*weftv1.PullImagesResponse, error) {
		got = in
		return &weftv1.PullImagesResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"--config-dir", "/custom", "--parallel", "10"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if got.ConfigDir != "/custom" || got.Parallel != 10 {
		t.Errorf("got = %+v", got)
	}
}

func TestPull_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.PullImagesFn = func(_ context.Context, _ *weftv1.PullImagesRequest) (*weftv1.PullImagesResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}
