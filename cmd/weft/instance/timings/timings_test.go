package timings

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
	if cmd.Use != "timings <name>" {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestTimings_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-timings.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestTimings_EmptyEventList(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMTimingsFn = func(_ context.Context, _ *vzdv1.VMTimingsRequest) (*vzdv1.VMTimingsResponse, error) {
		return &vzdv1.VMTimingsResponse{}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"alpha"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "no timings recorded") {
		t.Errorf("missing empty hint: %q", out)
	}
}

func TestTimings_HumanFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMTimingsFn = func(_ context.Context, _ *vzdv1.VMTimingsRequest) (*vzdv1.VMTimingsResponse, error) {
		return &vzdv1.VMTimingsResponse{Events: []*vzdv1.TimingEvent{
			// Out of order so sort.SliceStable runs.
			{Name: "server.start_attempted", TsUnixNs: 200000000, Meta: map[string]string{"mode": "direct_linux"}},
			{Name: "registered", TsUnixNs: 100000000},
			{Name: "server.vz_vm_run_forked", TsUnixNs: 300000000, Meta: map[string]string{"pid": "39666"}},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"vmname"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "registered") {
		t.Errorf("missing registered row: %q", out)
	}
	if !strings.Contains(out, "mode=direct_linux") {
		t.Errorf("missing meta: %q", out)
	}
	if !strings.Contains(out, "pid=39666") {
		t.Errorf("missing pid meta: %q", out)
	}
}

func TestTimings_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMTimingsFn = func(_ context.Context, _ *vzdv1.VMTimingsRequest) (*vzdv1.VMTimingsResponse, error) {
		return &vzdv1.VMTimingsResponse{Events: []*vzdv1.TimingEvent{
			{Name: "boot", TsUnixNs: 100, Meta: map[string]string{"k": "v"}},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"vm", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name": "boot"`) {
		t.Errorf("json missing name: %q", out)
	}
	if !strings.Contains(out, `"ts_unix_ns": 100`) {
		t.Errorf("json missing ts: %q", out)
	}
}

func TestTimings_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.VMTimingsFn = func(_ context.Context, _ *vzdv1.VMTimingsRequest) (*vzdv1.VMTimingsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"alpha"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

func TestFormatMeta_Empty(t *testing.T) {
	if got := formatMeta(nil); got != "" {
		t.Errorf("nil meta should be empty, got %q", got)
	}
	if got := formatMeta(map[string]string{}); got != "" {
		t.Errorf("empty meta should be empty, got %q", got)
	}
}

func TestFormatMeta_Sorted(t *testing.T) {
	got := formatMeta(map[string]string{"b": "2", "a": "1", "c": "3"})
	if got != "a=1 b=2 c=3" {
		t.Errorf("formatMeta sort = %q", got)
	}
}

func TestUnixNsToString(t *testing.T) {
	if got := unixNsToString(1_000_000_000); got != "1.000000000" {
		t.Errorf("unixNsToString(1e9) = %q", got)
	}
	if got := unixNsToString(1_500_000_001); got != "1.500000001" {
		t.Errorf("unixNsToString(1.5+1ns) = %q", got)
	}
}

func TestRenderTimeline_PicksLongestName(t *testing.T) {
	// Drive renderTimeline directly to make sure the maxNameLen
	// path runs even when no row has a meta map.
	out := captureStdout(t, func() {
		_ = renderTimeline([]*vzdv1.TimingEvent{
			{Name: "x", TsUnixNs: 1},
			{Name: "much-longer", TsUnixNs: 2},
		}, "vm")
	})
	if !strings.Contains(out, "much-longer") {
		t.Errorf("missing event name: %q", out)
	}
}
