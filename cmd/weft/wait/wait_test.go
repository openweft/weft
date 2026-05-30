package wait

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
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
	if cmd.Use != "wait <name>" {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestWait_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-wait.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestWait_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.WaitVMFn = func(_ context.Context, in *weftv1.WaitVMRequest) (*weftv1.WaitVMResponse, error) {
		if in.Name != "alpha" {
			t.Errorf("expected name=alpha, got %q", in.Name)
		}
		if in.TimeoutSeconds != 5 {
			t.Errorf("expected timeout=5, got %d", in.TimeoutSeconds)
		}
		return &weftv1.WaitVMResponse{Ip: "10.0.0.5"}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"alpha", "--timeout", "5"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "10.0.0.5") {
		t.Errorf("missing IP: %q", out)
	}
}

func TestWait_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.WaitVMFn = func(_ context.Context, _ *weftv1.WaitVMRequest) (*weftv1.WaitVMResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}
