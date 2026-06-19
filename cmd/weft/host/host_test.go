package host

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
	if cmd.Use != "host" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"register", "ls", "show", "set-state", "set-properties", "rm"}
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		parts := strings.SplitN(c.Use, " ", 2)
		got[parts[0]] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing sub %q (have %v)", w, got)
		}
	}
}

// ── register ────────────────────────────────────────────────────────────────

func TestRegister_RequiresHostname(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"register"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--hostname is required") {
		t.Errorf("expected hostname error, got %v", err)
	}
}

// TestAllSubcommands_DialError exercises the "shared.Client err"
// branch for every subcommand that has one. We run each as a
// t.Parallel sub-test so the 3 s weftclient dial deadline overlaps
// across goroutines — wall-clock stays ≈ 3 s instead of N×3 s.
func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"register", []string{"register", "--hostname", "h1"}},
		{"ls", []string{"ls"}},
		{"show", []string{"show", "u1"}},
		{"set-state", []string{"set-state", "u1", "active"}},
		{"set-properties", []string{"set-properties", "u1", "k=v"}},
		{"rm", []string{"rm", "u1"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-host-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s: expected dial error", c.name)
			}
		})
	}
}

func TestRegister_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.RegisterHostRequest
	srv.RegisterHostFn = func(_ context.Context, in *weftv1.RegisterHostRequest) (*weftv1.RegisterHostResponse, error) {
		got = in
		return &weftv1.RegisterHostResponse{Host: &weftv1.HostInfo{Uuid: "u1", Hostname: in.Hostname, Az: in.Az, Rack: in.Rack}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"register",
			"--uuid", "u1", "--hostname", "h1",
			"--az", "dc1", "--rack", "r1",
			"--endpoint", "h1:9090",
			"--hypervisor", "apple-vz", "--architecture", "arm64",
			"--network-types", "nat,bridged",
			"--volume-backends", "file",
			"--properties", "k1=v1,k2 = v2"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "registered host h1") {
		t.Errorf("expected confirmation in %q", out)
	}
	if got.Hostname != "h1" || got.Properties["k1"] != "v1" || got.Properties["k2"] != "v2" {
		t.Errorf("got = %+v", got)
	}
}

func TestRegister_PropertiesParseError(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"register", "--hostname", "h1", "--properties", "broken"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "is not k=v shape") {
		t.Errorf("expected property parse error, got %v", err)
	}
}

// TestRegister_LabelsDeprecatedAlias verifies the legacy --labels flag still
// works (forwarded to --properties) for one deprecation cycle.
func TestRegister_LabelsDeprecatedAlias(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.RegisterHostRequest
	srv.RegisterHostFn = func(_ context.Context, in *weftv1.RegisterHostRequest) (*weftv1.RegisterHostResponse, error) {
		got = in
		return &weftv1.RegisterHostResponse{Host: &weftv1.HostInfo{Uuid: "u1", Hostname: in.Hostname}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"register", "--hostname", "h1", "--labels", "k=v"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got == nil || got.Properties["k"] != "v" {
		t.Errorf("legacy --labels flag did not populate Properties: %+v", got)
	}
}

func TestRegister_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RegisterHostFn = func(_ context.Context, _ *weftv1.RegisterHostRequest) (*weftv1.RegisterHostResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"register", "--hostname", "h1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected rpc error")
	}
}

// ── ls ──────────────────────────────────────────────────────────────────────

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListHostsFn = func(_ context.Context, _ *weftv1.ListHostsRequest) (*weftv1.ListHostsResponse, error) {
		return &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u1", Hostname: "h1", Az: "dc1", Rack: "r1", Hypervisor: "apple-vz", Architecture: "arm64", State: "active", LastSeenAtUnixNs: 1_700_000_000_000_000_000},
				{Uuid: "u2", Hostname: "h2"}, // dash defaults
			},
			ConnectedHostUuids: []string{"u1"},
		}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--az", "dc1"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "h1") || !strings.Contains(out, "h2") {
		t.Errorf("missing rows: %q", out)
	}
	if !strings.Contains(out, "yes") {
		t.Errorf("missing connected=yes flag: %q", out)
	}
	if !strings.Contains(out, "no") {
		t.Errorf("missing connected=no flag: %q", out)
	}
}

func TestLs_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListHostsFn = func(_ context.Context, _ *weftv1.ListHostsRequest) (*weftv1.ListHostsResponse, error) {
		return &weftv1.ListHostsResponse{Hosts: []*weftv1.HostInfo{{Uuid: "u1", Hostname: "h1"}}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"uuid"`) {
		t.Errorf("json missing uuid: %q", out)
	}
}

func TestLs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListHostsFn = func(_ context.Context, _ *weftv1.ListHostsRequest) (*weftv1.ListHostsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── show ────────────────────────────────────────────────────────────────────

func TestShow_ByUUID(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetHostFn = func(_ context.Context, in *weftv1.GetHostRequest) (*weftv1.GetHostResponse, error) {
		if in.Uuid != "u1" || in.Hostname != "" {
			t.Errorf("expected UUID-mode, got %+v", in)
		}
		return &weftv1.GetHostResponse{Host: &weftv1.HostInfo{Uuid: "u1", Hostname: "h"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"show", "u1"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "u1") {
		t.Errorf("missing uuid: %q", out)
	}
}

func TestShow_ByHostname(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetHostFn = func(_ context.Context, in *weftv1.GetHostRequest) (*weftv1.GetHostResponse, error) {
		if in.Hostname != "myhost" || in.Uuid != "" {
			t.Errorf("expected hostname mode, got %+v", in)
		}
		return &weftv1.GetHostResponse{Host: &weftv1.HostInfo{Uuid: "x", Hostname: "myhost"}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"show", "myhost", "--by-hostname"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestShow_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.GetHostFn = func(_ context.Context, _ *weftv1.GetHostRequest) (*weftv1.GetHostResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"show", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── set-state ───────────────────────────────────────────────────────────────

func TestSetState_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.SetHostStateRequest
	srv.SetHostStateFn = func(_ context.Context, in *weftv1.SetHostStateRequest) (*weftv1.SetHostStateResponse, error) {
		got = in
		return &weftv1.SetHostStateResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-state", "u1", "draining"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if got.Uuid != "u1" || got.State != "draining" {
		t.Errorf("got = %+v", got)
	}
}

func TestSetState_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetHostStateFn = func(_ context.Context, _ *weftv1.SetHostStateRequest) (*weftv1.SetHostStateResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-state", "u1", "down"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── set-properties ──────────────────────────────────────────────────────────

func TestSetProperties_BadParse(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-properties", "u1", "broken"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected property parse error")
	}
}

func TestSetProperties_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.SetHostPropertiesRequest
	srv.SetHostPropertiesFn = func(_ context.Context, in *weftv1.SetHostPropertiesRequest) (*weftv1.SetHostPropertiesResponse, error) {
		got = in
		return &weftv1.SetHostPropertiesResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-properties", "u1", "k1=v1,k2=v2"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if got.Properties["k1"] != "v1" || got.Properties["k2"] != "v2" {
		t.Errorf("properties = %+v", got.Properties)
	}
}

func TestSetProperties_EmptyClears(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.SetHostPropertiesRequest
	srv.SetHostPropertiesFn = func(_ context.Context, in *weftv1.SetHostPropertiesRequest) (*weftv1.SetHostPropertiesResponse, error) {
		got = in
		return &weftv1.SetHostPropertiesResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-properties", "u1", ""})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if len(got.Properties) != 0 {
		t.Errorf("expected empty properties, got %v", got.Properties)
	}
}

func TestSetProperties_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetHostPropertiesFn = func(_ context.Context, _ *weftv1.SetHostPropertiesRequest) (*weftv1.SetHostPropertiesResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-properties", "u1", "k=v"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// TestSetLabels_DeprecatedAlias verifies the legacy `set-labels` cobra alias
// still drives the new SetHostProperties RPC for one deprecation cycle.
func TestSetLabels_DeprecatedAlias(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.SetHostPropertiesRequest
	srv.SetHostPropertiesFn = func(_ context.Context, in *weftv1.SetHostPropertiesRequest) (*weftv1.SetHostPropertiesResponse, error) {
		got = in
		return &weftv1.SetHostPropertiesResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-labels", "u1", "k=v"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got == nil || got.Properties["k"] != "v" {
		t.Errorf("legacy set-labels alias did not drive SetHostProperties: %+v", got)
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
	srv.DeleteHostFn = func(_ context.Context, _ *weftv1.DeleteHostRequest) (*weftv1.DeleteHostResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func TestParseProperties_VariousShapes(t *testing.T) {
	got, err := parseProperties("")
	if err != nil || len(got) != 0 {
		t.Errorf("empty: got=%v err=%v", got, err)
	}
	got, err = parseProperties("a=b, c = d ")
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if got["a"] != "b" || got["c"] != "d" {
		t.Errorf("trim failed: %v", got)
	}
	if _, err := parseProperties("noeq"); err == nil {
		t.Error("expected error for non k=v entry")
	}
	if _, err := parseProperties(" =empty-key"); err == nil {
		t.Error("expected empty-key error")
	}
}

func TestDashIf(t *testing.T) {
	if dashIf("") != "—" {
		t.Error("empty -> dash")
	}
	if dashIf("x") != "x" {
		t.Error("non-empty pass-through")
	}
}

func TestConnectedFlag(t *testing.T) {
	if connectedFlag(true) != "yes" {
		t.Error("true -> yes")
	}
	if connectedFlag(false) != "no" {
		t.Error("false -> no")
	}
}

func TestFormatTime(t *testing.T) {
	if got := formatTime(0); got != "—" {
		t.Errorf("zero -> %q, want dash", got)
	}
	if got := formatTime(1); got == "—" {
		t.Errorf("nonzero -> dash, got %q", got)
	}
}
