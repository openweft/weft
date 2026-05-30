package events

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
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
	if cmd.Use != "events" {
		t.Errorf("Use = %q", cmd.Use)
	}
	// Check the flags.
	for _, name := range []string{"kind-prefix", "project", "vm", "format"} {
		if cmd.Flag(name) == nil {
			t.Errorf("missing flag %q", name)
		}
	}
}

// TestEvents_DialError exercises the err-from-shared.Client branch.
func TestEvents_DialError(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-events.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

// TestEvents_StreamCompletes runs the events command against a test
// server that publishes 2 events then closes the stream. The
// resolver bootstrap calls ListProjects (we let testutil return the
// default zero-value response).
func TestEvents_StreamCompletes(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.WatchEventsFn = func(_ *weftv1.WatchEventsRequest, stream grpc.ServerStreamingServer[weftv1.PlatformEvent]) error {
		_ = stream.Send(&weftv1.PlatformEvent{TsUnixNs: 1700000000000000000, Kind: "vm.created", Subject: "alpha", ProjectUuid: "p1"})
		_ = stream.Send(&weftv1.PlatformEvent{TsUnixNs: 1700000000000000001, Kind: "vm.stopped", Subject: "alpha", ProjectUuid: "p1", Meta: map[string]string{"reason": "exit"}})
		return nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"--kind-prefix", "vm.", "--project", "p1", "--vm", "alpha"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "vm.created") {
		t.Errorf("missing event in output: %q", out)
	}
}

// TestEvents_JSONFormat exercises the json-format branch.
func TestEvents_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.WatchEventsFn = func(_ *weftv1.WatchEventsRequest, stream grpc.ServerStreamingServer[weftv1.PlatformEvent]) error {
		_ = stream.Send(&weftv1.PlatformEvent{Kind: "vm.started", Subject: "beta"})
		return nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"kind":"vm.started"`) {
		t.Errorf("missing json event: %q", out)
	}
}

// TestEvents_WatchError exercises the recv-error path.
func TestEvents_WatchError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.WatchEventsFn = func(_ *weftv1.WatchEventsRequest, _ grpc.ServerStreamingServer[weftv1.PlatformEvent]) error {
		return errors.New("server-side boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected stream error")
	}
}

// TestEvents_SignalCancel exercises the signal-notification-context
// path. We can't easily send SIGINT to ourselves without disrupting
// the test runner, so we trigger the same path by closing stdin and
// sending the process a SIGTERM via a sub-goroutine after the
// command starts. Simpler approach: server blocks until ctx is done,
// then we send the signal.
func TestEvents_SignalCancel(t *testing.T) {
	srv := testutil.NewServer(t)
	started := make(chan struct{})
	srv.WatchEventsFn = func(_ *weftv1.WatchEventsRequest, stream grpc.ServerStreamingServer[weftv1.PlatformEvent]) error {
		close(started)
		<-stream.Context().Done()
		return stream.Context().Err()
	}

	// Use a sub-process trick: actually, simpler — send SIGTERM
	// to our own process group after the stream is established.
	// The signal.NotifyContext should catch it and cancel.
	done := make(chan error, 1)
	go func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{})
		done <- cmd.Execute()
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server WatchEvents never invoked")
	}
	// Send SIGTERM to ourselves; signal.NotifyContext catches it
	// and ctx.Done() fires inside StreamEvents → graceful exit
	// (returns nil because ctx.Err() != nil).
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case err := <-done:
		// Either nil (caught and swallowed) or a context-cancellation
		// wrap; both are acceptable since this is a graceful-shutdown
		// path. Don't be strict about the value.
		_ = err
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after SIGTERM")
	}
}

// Silence unused exec import in case the SIGTERM hack is not picked.
var _ = exec.Command
var _ = context.Background
