package volume

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
	if cmd.Use != "volume" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "create", "rename", "resize", "attach", "detach", "rm", "snapshot"}
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		got[strings.SplitN(c.Use, " ", 2)[0]] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

// ── ls ──────────────────────────────────────────────────────────────────────

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVolumesFn = func(_ context.Context, _ *weftv1.ListVolumesRequest) (*weftv1.ListVolumesResponse, error) {
		return &weftv1.ListVolumesResponse{Volumes: []*weftv1.VolumeInfo{
			{Uuid: "u1", ProjectUuid: "p1", Name: "vol", SizeGib: 10, Format: "raw", AttachedToUuid: "vm-1", CreatedAtUnixNs: 1700000000000000000},
			{Uuid: "u2", ProjectUuid: "p2", Name: "vol2", SizeGib: 5, Format: "qcow2"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "vol") || !strings.Contains(out, "vm-1") {
		t.Errorf("missing rows: %q", out)
	}
}

func TestLs_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVolumesFn = func(_ context.Context, _ *weftv1.ListVolumesRequest) (*weftv1.ListVolumesResponse, error) {
		return &weftv1.ListVolumesResponse{Volumes: []*weftv1.VolumeInfo{
			{Uuid: "u1", Name: "vol", SizeGib: 10, Format: "raw"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--format", "json", "--project", "p"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name": "vol"`) {
		t.Errorf("missing json: %q", out)
	}
}

func TestLs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVolumesFn = func(_ context.Context, _ *weftv1.ListVolumesRequest) (*weftv1.ListVolumesResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── create ──────────────────────────────────────────────────────────────────

func TestCreate_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.CreateVolumeRequest
	srv.CreateVolumeFn = func(_ context.Context, in *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error) {
		got = in
		return &weftv1.CreateVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: "u1", Name: in.Name, SizeGib: in.SizeGib, Format: in.Format}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"create", "--project", "p", "--name", "vol", "--size-gib", "20", "--format", "raw"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if got.SizeGib != 20 || got.Name != "vol" {
		t.Errorf("got = %+v", got)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("missing banner: %q", out)
	}
}

func TestCreate_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateVolumeFn = func(_ context.Context, _ *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "--name", "v", "--size-gib", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── rename ──────────────────────────────────────────────────────────────────

func TestRename_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenameVolumeFn = func(_ context.Context, in *weftv1.RenameVolumeRequest) (*weftv1.RenameVolumeResponse, error) {
		return &weftv1.RenameVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "u1", "new"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestRename_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenameVolumeFn = func(_ context.Context, _ *weftv1.RenameVolumeRequest) (*weftv1.RenameVolumeResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "u1", "new"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── resize ──────────────────────────────────────────────────────────────────

func TestResize_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ResizeVolumeFn = func(_ context.Context, in *weftv1.ResizeVolumeRequest) (*weftv1.ResizeVolumeResponse, error) {
		return &weftv1.ResizeVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: in.Uuid, SizeGib: in.NewSizeGib}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"resize", "u1", "30"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestResize_ZeroSize(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"resize", "u1", "0"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Errorf("expected positive-integer error, got %v", err)
	}
}

func TestResize_NotANumber(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"resize", "u1", "huge"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestResize_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ResizeVolumeFn = func(_ context.Context, _ *weftv1.ResizeVolumeRequest) (*weftv1.ResizeVolumeResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"resize", "u1", "5"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── attach ──────────────────────────────────────────────────────────────────

func TestAttach_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.AttachVolumeFn = func(_ context.Context, in *weftv1.AttachVolumeRequest) (*weftv1.AttachVolumeResponse, error) {
		return &weftv1.AttachVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: in.Uuid, AttachedToUuid: in.VmUuid}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"attach", "u1", "vm-1"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestAttach_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.AttachVolumeFn = func(_ context.Context, _ *weftv1.AttachVolumeRequest) (*weftv1.AttachVolumeResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"attach", "u1", "vm-1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── detach ──────────────────────────────────────────────────────────────────

func TestDetach_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"detach", "u1"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestDetach_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DetachVolumeFn = func(_ context.Context, _ *weftv1.DetachVolumeRequest) (*weftv1.DetachVolumeResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"detach", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── rm ──────────────────────────────────────────────────────────────────────

func TestRm_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "u1"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestRm_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteVolumeFn = func(_ context.Context, _ *weftv1.DeleteVolumeRequest) (*weftv1.DeleteVolumeResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── dial errors ─────────────────────────────────────────────────────────────

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"create", []string{"create", "--name", "v", "--size-gib", "1"}},
		{"rename", []string{"rename", "u1", "n"}},
		{"resize", []string{"resize", "u1", "5"}},
		{"attach", []string{"attach", "u1", "vm-1"}},
		{"detach", []string{"detach", "u1"}},
		{"rm", []string{"rm", "u1"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-vol-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s: expected dial error", c.name)
			}
		})
	}
}
