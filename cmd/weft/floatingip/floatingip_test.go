package floatingip

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "floating-ip" {
		t.Errorf("root.Use : got %q, want floating-ip", root.Use)
	}
	want := map[string]bool{
		"ls":               false,
		"show <uuid>":      false,
		"status [<uuid>]":  false,
		"allocate":         false,
		"release <uuid>":   false,
		"map <uuid>":       false,
		"unmap <uuid>":     false,
	}
	for _, sub := range root.Commands() {
		want[sub.Use] = true
	}
	for use, seen := range want {
		if !seen {
			t.Errorf("missing subcommand %q", use)
		}
	}
}

func TestAllocateCmd_Flags(t *testing.T) {
	cmd := allocateCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"project", "network"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestMapCmd_Flags(t *testing.T) {
	cmd := mapCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"kind", "target"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"show", []string{"show", "00000000-0000-0000-0000-000000000001"}},
		{"status", []string{"status", "00000000-0000-0000-0000-000000000001"}},
		{"allocate", []string{"allocate", "--network", "edge"}},
		{"release", []string{"release", "00000000-0000-0000-0000-000000000001"}},
		{"map", []string{"map", "00000000-0000-0000-0000-000000000001", "--target", "vm1"}},
		{"unmap", []string{"unmap", "00000000-0000-0000-0000-000000000001"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-fip-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error", c.name)
			}
		})
	}
}

func TestAllocateCmd_MissingNetwork(t *testing.T) {
	cmd := Command(strPtr(""), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"allocate"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --network is missing")
	}
}

func TestMapCmd_MissingTarget(t *testing.T) {
	cmd := Command(strPtr(""), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"map", "00000000-0000-0000-0000-000000000001"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --target is missing")
	}
}

var _ = cobra.NoArgs

// stubAgent is a minimal in-test WeftAgentServer that lets us drive
// the `status` verb end-to-end without depending on the larger
// cmd/weft/internal/testutil harness (which doesn't yet expose
// ListFloatingIPs/VMStatus hooks). The two RPCs the verb actually
// calls — ListFloatingIPs and VMStatus — are pluggable ; everything
// else inherits the Unimplemented default and would return an error
// if the verb accidentally widened its dependency surface.
type stubAgent struct {
	weftv1.UnimplementedWeftAgentServer
	listFn   func(context.Context, *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error)
	statusFn func(context.Context, *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error)
}

func (s *stubAgent) ListFloatingIPs(ctx context.Context, in *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error) {
	if s.listFn != nil {
		return s.listFn(ctx, in)
	}
	return &weftv1.ListFloatingIPsResponse{}, nil
}

func (s *stubAgent) VMStatus(ctx context.Context, in *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error) {
	if s.statusFn != nil {
		return s.statusFn(ctx, in)
	}
	return &weftv1.VMStatusResponse{Vm: &weftv1.VMInfo{Name: in.Name}}, nil
}

// startStubAgent spins a grpc.Server on a unix socket under t.TempDir
// and registers the stub. Returns the socket path ; t.Cleanup tears
// down the server. Mirrors the layout of cmd/weft/internal/testutil
// but lives here so this package keeps its own narrow scope.
func startStubAgent(t *testing.T, st *stubAgent) string {
	t.Helper()
	// Short paths only — darwin caps unix socket paths at ~104 chars.
	socket := filepath.Join("/tmp", "weft-fipstatus-"+time.Now().Format("150405.000000000")+".sock")
	srv := grpc.NewServer()
	weftv1.RegisterWeftAgentServer(srv, st)
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.GracefulStop()
		_ = os.Remove(socket)
	})
	// Tiny warm-up so dials don't race the listener.
	time.Sleep(5 * time.Millisecond)
	return socket
}

// captureStdout redirects os.Stdout for the duration of fn so we can
// assert on the rendered output. Same shape as microvm_test.go.
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

const fipUUID = "00000000-0000-0000-0000-0000000000aa"

func fipFixture(mappedTo string) *weftv1.FloatingIPInfo {
	st := "active"
	if mappedTo == "" {
		st = "available"
	}
	return &weftv1.FloatingIPInfo{
		Uuid:              fipUUID,
		Address:           "203.0.113.7",
		Network:           "edge",
		ProjectUuid:       "proj-uuid",
		MappedTo:          mappedTo,
		Status:            st,
		AllocatedAtUnixNs: time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC).UnixNano(),
	}
}

// TestStatusCmd_Active : FIP mapped to a VM whose IP resolves. The
// host-side block must show the dnat/snat pair derived from the FIP
// address + the VM's private IP.
func TestStatusCmd_Active(t *testing.T) {
	st := &stubAgent{
		listFn: func(_ context.Context, _ *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error) {
			return &weftv1.ListFloatingIPsResponse{
				FloatingIps: []*weftv1.FloatingIPInfo{fipFixture("web-1")},
			}, nil
		},
		statusFn: func(_ context.Context, in *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error) {
			if in.Name != "web-1" {
				t.Errorf("VMStatus.Name : got %q, want web-1", in.Name)
			}
			return &weftv1.VMStatusResponse{Vm: &weftv1.VMInfo{Name: in.Name, Ip: "10.42.0.5"}}, nil
		},
	}
	sock := startStubAgent(t, st)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(sock), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"status", fipUUID})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	wantPieces := []string{
		fipUUID,
		"203.0.113.7",
		"HOST:",
		"nftables expected: dnat=203.0.113.7→10.42.0.5",
		"snat=10.42.0.5→203.0.113.7",
	}
	for _, p := range wantPieces {
		if !strings.Contains(out, p) {
			t.Errorf("output missing %q\n--- got ---\n%s", p, out)
		}
	}
}

// TestStatusCmd_Unmapped : FIP exists but mapped_to is empty. The
// host-side block must announce the "unmapped" branch.
func TestStatusCmd_Unmapped(t *testing.T) {
	st := &stubAgent{
		listFn: func(_ context.Context, _ *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error) {
			return &weftv1.ListFloatingIPsResponse{
				FloatingIps: []*weftv1.FloatingIPInfo{fipFixture("")},
			}, nil
		},
		statusFn: func(_ context.Context, _ *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error) {
			t.Error("VMStatus should not be called for an unmapped FIP")
			return &weftv1.VMStatusResponse{}, nil
		},
	}
	sock := startStubAgent(t, st)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(sock), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"status", fipUUID})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "not yet active (FIP unmapped)") {
		t.Errorf("unmapped branch missing\n%s", out)
	}
}

// TestStatusCmd_VMUnreachable : FIP mapped to a name VMStatus errors
// on (remote-host VM, LB target, …). Should fall through to the
// "VM not local" branch without crashing.
func TestStatusCmd_VMUnreachable(t *testing.T) {
	st := &stubAgent{
		listFn: func(_ context.Context, _ *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error) {
			return &weftv1.ListFloatingIPsResponse{
				FloatingIps: []*weftv1.FloatingIPInfo{fipFixture("ghost-lb")},
			}, nil
		},
		statusFn: func(_ context.Context, _ *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error) {
			// Empty IP — VM exists in the registry but isn't
			// running on this host, or has no port attachment yet.
			return &weftv1.VMStatusResponse{Vm: &weftv1.VMInfo{Name: "ghost-lb"}}, nil
		},
	}
	sock := startStubAgent(t, st)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(sock), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"status", fipUUID})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `VM not local to any visible host (target="ghost-lb")`) {
		t.Errorf("vm-unreachable branch missing\n%s", out)
	}
}

// TestStatusCmd_NoArg : `weft floating-ip status` with no argument
// behaves like `ls` — renders the full table, no host-side block.
func TestStatusCmd_NoArg(t *testing.T) {
	st := &stubAgent{
		listFn: func(_ context.Context, _ *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error) {
			return &weftv1.ListFloatingIPsResponse{
				FloatingIps: []*weftv1.FloatingIPInfo{fipFixture("web-1"), fipFixture("")},
			}, nil
		},
	}
	sock := startStubAgent(t, st)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(sock), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"status"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "203.0.113.7") {
		t.Errorf("table row missing\n%s", out)
	}
	if strings.Contains(out, "HOST:") {
		t.Errorf("HOST: block should not render without a uuid arg\n%s", out)
	}
}

// TestStatusCmd_NotFound : asked for a UUID that ListFloatingIPs
// doesn't return → cobra surfaces the error.
func TestStatusCmd_NotFound(t *testing.T) {
	st := &stubAgent{
		listFn: func(_ context.Context, _ *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error) {
			return &weftv1.ListFloatingIPsResponse{}, nil
		},
	}
	sock := startStubAgent(t, st)
	cmd := Command(strPtr(sock), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"status", "missing-uuid"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown uuid")
	}
	if !strings.Contains(err.Error(), "missing-uuid") {
		t.Errorf("error should mention the queried key : %v", err)
	}
}

// TestStatusCmd_JSON : --format json emits the flat shape with a
// "host" sub-object. Spot-check the key bits ; full schema asserted
// implicitly by json.Encoder.
func TestStatusCmd_JSON(t *testing.T) {
	st := &stubAgent{
		listFn: func(_ context.Context, _ *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error) {
			return &weftv1.ListFloatingIPsResponse{
				FloatingIps: []*weftv1.FloatingIPInfo{fipFixture("web-1")},
			}, nil
		},
		statusFn: func(_ context.Context, in *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error) {
			return &weftv1.VMStatusResponse{Vm: &weftv1.VMInfo{Name: in.Name, Ip: "10.42.0.5"}}, nil
		},
	}
	sock := startStubAgent(t, st)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(sock), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"status", fipUUID, "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	wantPieces := []string{
		`"uuid": "` + fipUUID + `"`,
		`"branch": "active"`,
		`"vm_ip": "10.42.0.5"`,
		`"address": "203.0.113.7"`,
	}
	for _, p := range wantPieces {
		if !strings.Contains(out, p) {
			t.Errorf("json missing %q\n%s", p, out)
		}
	}
}
