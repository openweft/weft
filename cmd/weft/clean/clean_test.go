package clean

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
	if cmd.Use != "clean" {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestClean_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-clean.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestClean_DryRunListsAndAnnotates(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CleanImagesFn = func(_ context.Context, in *weftv1.CleanImagesRequest) (*weftv1.CleanImagesResponse, error) {
		if !in.DryRun {
			t.Errorf("expected dry-run when --yes not set, got DryRun=%v", in.DryRun)
		}
		return &weftv1.CleanImagesResponse{Deleted: []string{"img-a", "img-b"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run banner missing: %q", out)
	}
	if !strings.Contains(out, "img-a") {
		t.Errorf("img-a missing: %q", out)
	}
}

func TestClean_ConfirmedDeletesNoBanner(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CleanImagesFn = func(_ context.Context, in *weftv1.CleanImagesRequest) (*weftv1.CleanImagesResponse, error) {
		if in.DryRun {
			t.Errorf("expected DryRun=false when --yes was set")
		}
		return &weftv1.CleanImagesResponse{Deleted: []string{"img-z"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"--yes"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if strings.Contains(out, "dry-run") {
		t.Errorf("dry-run banner should not appear: %q", out)
	}
	if !strings.Contains(out, "img-z") {
		t.Errorf("img-z missing: %q", out)
	}
}

func TestClean_NothingToCleanMessage(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CleanImagesFn = func(_ context.Context, _ *weftv1.CleanImagesRequest) (*weftv1.CleanImagesResponse, error) {
		return &weftv1.CleanImagesResponse{}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"--yes"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "nothing to clean") {
		t.Errorf("empty response missing 'nothing to clean': %q", out)
	}
}

func TestClean_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CleanImagesFn = func(_ context.Context, _ *weftv1.CleanImagesRequest) (*weftv1.CleanImagesResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected rpc error")
	}
}
