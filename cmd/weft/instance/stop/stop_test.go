package stop

import (
	"context"
	"errors"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	vzdv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "stop <name>" {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestStop_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-stop.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestStop_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	got := ""
	srv.StopVMFn = func(_ context.Context, in *vzdv1.StopVMRequest) (*vzdv1.StopVMResponse, error) {
		got = in.Name
		return &vzdv1.StopVMResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"beta"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if got != "beta" {
		t.Errorf("name = %q", got)
	}
}

func TestStop_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.StopVMFn = func(_ context.Context, _ *vzdv1.StopVMRequest) (*vzdv1.StopVMResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"beta"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}
