package logs

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

// captureOutputs swaps both stdout and stderr so we can read each
// separately — the logs command splits the log body (stdout) and a
// truncation hint (stderr) on purpose.
func captureOutputs(t *testing.T, fn func()) (string, string) {
	t.Helper()
	stdoutR, stdoutW, _ := os.Pipe()
	stderrR, stderrW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutW, stderrW
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	outDone := make(chan string, 1)
	errDone := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, stdoutR)
		outDone <- b.String()
	}()
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, stderrR)
		errDone <- b.String()
	}()
	fn()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	return <-outDone, <-errDone
}

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "logs <name>" {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestLogs_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-logs.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestLogs_FullLogToStdout(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMLogsFn = func(_ context.Context, _ *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		return &weftv1.VMLogsResponse{Contents: []byte("boot ok\n"), TotalBytes: 8}, nil
	}
	stdout, stderr := captureOutputs(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"alpha"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(stdout, "boot ok") {
		t.Errorf("stdout missing log body: %q", stdout)
	}
	if strings.Contains(stderr, "truncated") {
		t.Errorf("unexpected truncation note when tail=0: %q", stderr)
	}
}

func TestLogs_TailWithTruncation(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMLogsFn = func(_ context.Context, in *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		if in.TailBytes != 5 {
			t.Errorf("tail = %d", in.TailBytes)
		}
		return &weftv1.VMLogsResponse{Contents: []byte("hello"), TotalBytes: 100}, nil
	}
	stdout, stderr := captureOutputs(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"alpha", "--tail", "5"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(stdout, "hello") {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "truncated") {
		t.Errorf("stderr missing truncation hint: %q", stderr)
	}
}

// TestLogs_StdoutWriteError closes os.Stdout before invoking the
// command so the os.Stdout.Write returns an error — pins the err
// branch at the end of the RunE.
func TestLogs_StdoutWriteError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMLogsFn = func(_ context.Context, _ *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		return &weftv1.VMLogsResponse{Contents: []byte("hello"), TotalBytes: 5}, nil
	}
	// Replace stdout with the read end of a pipe and close it
	// immediately. Subsequent writes return EPIPE.
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	_ = r.Close()
	_ = w.Close() // close write side so any further Write fails
	defer func() { os.Stdout = old }()

	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected stdout write error")
	}
}

func TestLogs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMLogsFn = func(_ context.Context, _ *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}
