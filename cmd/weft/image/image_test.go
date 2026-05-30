package image

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
	if cmd.Use != "image" {
		t.Errorf("Use = %q", cmd.Use)
	}
	subs := map[string]bool{}
	for _, c := range cmd.Commands() {
		parts := strings.SplitN(c.Use, " ", 2)
		subs[parts[0]] = true
	}
	if !subs["list"] || !subs["pull"] {
		t.Errorf("expected list+pull subcommands, got %v", subs)
	}
}

func TestList_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-img.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestList_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListImagesFn = func(_ context.Context, _ *weftv1.ListImagesRequest) (*weftv1.ListImagesResponse, error) {
		return &weftv1.ListImagesResponse{Images: []*weftv1.ImageInfo{
			{Name: "ubuntu", Format: "qcow2", Url: "u", SizeBytes: 1024},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"list"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "ubuntu") {
		t.Errorf("table missing ubuntu: %q", out)
	}
}

func TestList_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListImagesFn = func(_ context.Context, _ *weftv1.ListImagesRequest) (*weftv1.ListImagesResponse, error) {
		return &weftv1.ListImagesResponse{Images: []*weftv1.ImageInfo{
			{Name: "ubuntu", Format: "qcow2", Url: "u", SizeBytes: 1024},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"list", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name":"ubuntu"`) {
		t.Errorf("missing json row: %q", out)
	}
}

func TestList_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListImagesFn = func(_ context.Context, _ *weftv1.ListImagesRequest) (*weftv1.ListImagesResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}
