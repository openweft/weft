package admin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

// strPtr is a tiny helper. The Command signature takes *string for
// the socket pointers so the global cobra root can mutate them
// after definition; tests construct fresh pointers per call.
func strPtr(s string) *string { return &s }

// captureStdout drains the process-wide stdout into a string for
// the duration of fn. Necessary because the admin command writes
// the rendered authorisation block directly to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
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
	if cmd.Use != "admin" {
		t.Errorf("Use = %q, want admin", cmd.Use)
	}
	if cmd.Short == "" {
		t.Error("Short empty")
	}
	if len(cmd.Commands()) == 0 {
		t.Fatal("admin should have sub-commands")
	}
	var found bool
	for _, sub := range cmd.Commands() {
		if sub.Use == "nats-authz" {
			found = true
			break
		}
	}
	if !found {
		t.Error("missing nats-authz subcommand")
	}
}

func TestNatsAuthz_BadSocketReturnsError(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-admin-bad.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"nats-authz"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error with bogus socket")
	}
}

func TestNatsAuthz_StdoutSuccess(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenderNATSAuthorizationFn = func(_ context.Context, _ *weftv1.RenderNATSAuthorizationRequest) (*weftv1.RenderNATSAuthorizationResponse, error) {
		return &weftv1.RenderNATSAuthorizationResponse{Config: []byte("authorization { hello }")}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"nats-authz"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "authorization { hello }") {
		t.Errorf("missing rendered block in stdout: %q", out)
	}
}

func TestNatsAuthz_WritesToOutFile(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenderNATSAuthorizationFn = func(_ context.Context, _ *weftv1.RenderNATSAuthorizationRequest) (*weftv1.RenderNATSAuthorizationResponse, error) {
		return &weftv1.RenderNATSAuthorizationResponse{Config: []byte("hi")}, nil
	}
	tmp := filepath.Join(t.TempDir(), "out.conf")
	// Capture stderr too — the command logs the file write to stderr.
	oldErr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	defer func() { os.Stderr = oldErr }()

	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"nats-authz", "--out", tmp, "--admin-pubkey", "UAAA"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	if !strings.Contains(buf.String(), "wrote 2 bytes") {
		t.Errorf("missing 'wrote' stderr log: %q", buf.String())
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	if string(b) != "hi" {
		t.Errorf("file = %q", string(b))
	}
}

func TestNatsAuthz_OutFileWriteError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenderNATSAuthorizationFn = func(_ context.Context, _ *weftv1.RenderNATSAuthorizationRequest) (*weftv1.RenderNATSAuthorizationResponse, error) {
		return &weftv1.RenderNATSAuthorizationResponse{Config: []byte("hi")}, nil
	}
	// Target a path whose parent doesn't exist → write fails.
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"nats-authz", "--out", "/no/such/dir/nope.conf"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "write") {
		t.Errorf("expected write error, got %v", err)
	}
}

func TestNatsAuthz_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenderNATSAuthorizationFn = func(_ context.Context, _ *weftv1.RenderNATSAuthorizationRequest) (*weftv1.RenderNATSAuthorizationResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"nats-authz"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "RenderNATSAuthorization") {
		t.Errorf("err = %v", err)
	}
}
