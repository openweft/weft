package start

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
	if cmd.Use != "start <name>" {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestStart_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-start.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestStart_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	got := ""
	srv.StartVMFn = func(_ context.Context, in *vzdv1.StartVMRequest) (*vzdv1.StartVMResponse, error) {
		got = in.Name
		return &vzdv1.StartVMResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if got != "alpha" {
		t.Errorf("name = %q", got)
	}
}

func TestStart_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.StartVMFn = func(_ context.Context, _ *vzdv1.StartVMRequest) (*vzdv1.StartVMResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}
