package flavor

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

// ── structure ───────────────────────────────────────────────────────────────

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "flavor" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "get", "set", "rm"}
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
	srv.ListFlavorsFn = func(_ context.Context, _ *weftv1.ListFlavorsRequest) (*weftv1.ListFlavorsResponse, error) {
		return &weftv1.ListFlavorsResponse{Flavors: []*weftv1.Flavor{
			{Name: "small", Vcpu: 2, Ram: "4Gi", EphemeralGb: 20},
			{Name: "gpu", Vcpu: 8, Ram: "32Gi", EphemeralGb: 100, Gpu: "1×A100-40G"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	for _, want := range []string{"NAME", "small", "4Gi", "gpu", "1×A100-40G"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q in :\n%s", want, out)
		}
	}
	// "-" renders for a flavor with no GPU. Tabwriter padding makes
	// the column width depend on the longest row's GPU value, so we
	// just look for the "small" row and confirm it ends in "-".
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "small") && !strings.HasSuffix(strings.TrimRight(line, " "), "-") {
			t.Errorf("small row should end in '-' (no GPU) : %q", line)
		}
	}
}

func TestLs_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListFlavorsFn = func(_ context.Context, _ *weftv1.ListFlavorsRequest) (*weftv1.ListFlavorsResponse, error) {
		return &weftv1.ListFlavorsResponse{Flavors: []*weftv1.Flavor{
			{Name: "small", Vcpu: 2, Ram: "4Gi"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--format=json"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name": "small"`) {
		t.Errorf("expected JSON name field in :\n%s", out)
	}
	if !strings.Contains(out, `"vcpu": 2`) {
		t.Errorf("expected JSON vcpu field in :\n%s", out)
	}
}

// ── get ─────────────────────────────────────────────────────────────────────

func TestGet_PassesName(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen string
	srv.GetFlavorFn = func(_ context.Context, in *weftv1.GetFlavorRequest) (*weftv1.GetFlavorResponse, error) {
		seen = in.Name
		return &weftv1.GetFlavorResponse{Flavor: &weftv1.Flavor{Name: in.Name, Vcpu: 4, Ram: "8Gi"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"get", "medium"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen != "medium" {
		t.Errorf("server got name=%q, want medium", seen)
	}
	if !strings.Contains(out, "medium") || !strings.Contains(out, "8Gi") {
		t.Errorf("output missing fields in :\n%s", out)
	}
}

// ── set ─────────────────────────────────────────────────────────────────────

func TestSet_RoundTrip(t *testing.T) {
	srv := testutil.NewServer(t)
	var saved *weftv1.Flavor
	srv.SetFlavorFn = func(_ context.Context, in *weftv1.SetFlavorRequest) (*weftv1.SetFlavorResponse, error) {
		saved = in.Flavor
		return &weftv1.SetFlavorResponse{Flavor: in.Flavor}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set", "gpu", "--vcpu=8", "--ram=32Gi", "--ephemeral-gb=100", "--gpu=1×A100-40G"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if saved == nil || saved.Name != "gpu" || saved.Vcpu != 8 || saved.Ram != "32Gi" || saved.EphemeralGb != 100 || saved.Gpu != "1×A100-40G" {
		t.Errorf("server saw %+v, fields didn't all transit", saved)
	}
	if !strings.Contains(out, "set\tgpu") {
		t.Errorf("expected 'set\\tgpu' prefix in :\n%s", out)
	}
}

func TestSet_ErrorBubblesUp(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetFlavorFn = func(_ context.Context, _ *weftv1.SetFlavorRequest) (*weftv1.SetFlavorResponse, error) {
		return nil, errors.New("vcpu must be > 0")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set", "bad", "--vcpu=0", "--ram=1Gi"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error, got nil")
	} else if !strings.Contains(err.Error(), "vcpu must be > 0") {
		t.Errorf("error %q should mention vcpu", err)
	}
}

// ── rm ──────────────────────────────────────────────────────────────────────

func TestRm_PassesName(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen string
	srv.DeleteFlavorFn = func(_ context.Context, in *weftv1.DeleteFlavorRequest) (*weftv1.DeleteFlavorResponse, error) {
		seen = in.Name
		return &weftv1.DeleteFlavorResponse{Deleted: in.Name}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "old-flavor"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen != "old-flavor" {
		t.Errorf("server got name=%q, want old-flavor", seen)
	}
	if !strings.Contains(out, "deleted\told-flavor") {
		t.Errorf("expected 'deleted\\told-flavor' in :\n%s", out)
	}
}
