package instance

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

func TestCommand_StructureHasAllSubcommands(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "instance" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"list", "start", "stop", "status", "register-microvm", "timings", "logs"}
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		// First word only — sub-commands include `<name>` placeholders.
		parts := strings.SplitN(c.Use, " ", 2)
		got[parts[0]] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing subcommand %q (have %v)", w, got)
		}
	}
}

func TestList_BadSocketErrors(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-instance.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestList_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMsFn = func(_ context.Context, _ *vzdv1.ListVMsRequest) (*vzdv1.ListVMsResponse, error) {
		return &vzdv1.ListVMsResponse{Vms: []*vzdv1.VMInfo{
			{Name: "alpha", State: vzdv1.VMState_VM_STATE_RUNNING, Os: "linux", Cpu: 2, MemMb: 1024, DiskGb: 10, Ip: "10.0.0.2"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"list"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "alpha") {
		t.Errorf("missing alpha: %q", out)
	}
}

func TestList_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMsFn = func(_ context.Context, _ *vzdv1.ListVMsRequest) (*vzdv1.ListVMsResponse, error) {
		return &vzdv1.ListVMsResponse{Vms: []*vzdv1.VMInfo{
			{Name: "alpha", State: vzdv1.VMState_VM_STATE_RUNNING, Ip: "10.0.0.2"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"list", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name":"alpha"`) {
		t.Errorf("json missing: %q", out)
	}
}

func TestList_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMsFn = func(_ context.Context, _ *vzdv1.ListVMsRequest) (*vzdv1.ListVMsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}
