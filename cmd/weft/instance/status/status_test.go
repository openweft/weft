package status

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
	if cmd.Use != "status <name>" {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestStatus_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-status.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestStatus_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMStatusFn = func(_ context.Context, in *vzdv1.VMStatusRequest) (*vzdv1.VMStatusResponse, error) {
		return &vzdv1.VMStatusResponse{Vm: &vzdv1.VMInfo{Name: in.Name, State: vzdv1.VMState_VM_STATE_RUNNING}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"alpha"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "alpha") {
		t.Errorf("missing alpha: %q", out)
	}
}

func TestStatus_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMStatusFn = func(_ context.Context, _ *vzdv1.VMStatusRequest) (*vzdv1.VMStatusResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}
