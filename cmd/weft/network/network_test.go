package network

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
	if cmd.Use != "network" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "create", "rename", "set-dns", "set-sgs", "rm"}
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

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"create", []string{"create", "--name", "n", "--cidr", "10.0.0.0/24"}},
		{"rename", []string{"rename", "u1", "new"}},
		{"set-dns", []string{"set-dns", "u1", "1.1.1.1"}},
		{"set-sgs", []string{"set-sgs", "u1", "u2"}},
		{"rm", []string{"rm", "u1"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-net-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s: expected dial error", c.name)
			}
		})
	}
}

// ── ls ──────────────────────────────────────────────────────────────────────

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListNetworksFn = func(_ context.Context, _ *weftv1.ListNetworksRequest) (*weftv1.ListNetworksResponse, error) {
		return &weftv1.ListNetworksResponse{Networks: []*weftv1.NetworkInfo{
			{Uuid: "u1", ProjectUuid: "p1", Name: "net", Cidr: "10.0.0.0/24", Gateway: "10.0.0.1", Type: "nat", DnsServers: []string{"1.1.1.1"}, DefaultSecurityGroupUuids: []string{"sg1"}, CreatedAtUnixNs: 1700000000000000000},
			{Uuid: "u2", ProjectUuid: "p2", Name: "net2", Cidr: "10.1.0.0/24"}, // gateway/dns/sgs empty -> dash branch
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "net") || !strings.Contains(out, "10.0.0.0/24") {
		t.Errorf("missing rows: %q", out)
	}
}

func TestLs_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListNetworksFn = func(_ context.Context, _ *weftv1.ListNetworksRequest) (*weftv1.ListNetworksResponse, error) {
		return &weftv1.ListNetworksResponse{Networks: []*weftv1.NetworkInfo{
			{Uuid: "u1", Name: "n", Cidr: "10.0.0.0/24", Type: "nat"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--format", "json", "--project", "p"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"uuid": "u1"`) {
		t.Errorf("json missing uuid: %q", out)
	}
}

func TestLs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListNetworksFn = func(_ context.Context, _ *weftv1.ListNetworksRequest) (*weftv1.ListNetworksResponse, error) {
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
	var got *weftv1.CreateNetworkRequest
	srv.CreateNetworkFn = func(_ context.Context, in *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error) {
		got = in
		return &weftv1.CreateNetworkResponse{Network: &weftv1.NetworkInfo{Uuid: "u1", Name: in.Name, Cidr: in.Cidr, Type: in.Type}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"create",
			"--project", "p", "--name", "n", "--cidr", "10.0.0.0/24",
			"--gateway", "10.0.0.1", "--dns", "1.1.1.1,8.8.8.8", "--type", "nat"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "created") {
		t.Errorf("missing created banner: %q", out)
	}
	if got.Cidr != "10.0.0.0/24" || got.Gateway != "10.0.0.1" || len(got.DnsServers) != 2 {
		t.Errorf("got = %+v", got)
	}
}

func TestCreate_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateNetworkFn = func(_ context.Context, _ *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "--name", "n", "--cidr", "10.0.0.0/24"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── rename ──────────────────────────────────────────────────────────────────

func TestRename_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenameNetworkFn = func(_ context.Context, in *weftv1.RenameNetworkRequest) (*weftv1.RenameNetworkResponse, error) {
		return &weftv1.RenameNetworkResponse{Network: &weftv1.NetworkInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rename", "u1", "newname"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "renamed") {
		t.Errorf("missing banner: %q", out)
	}
}

func TestRename_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenameNetworkFn = func(_ context.Context, _ *weftv1.RenameNetworkRequest) (*weftv1.RenameNetworkResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "u1", "new"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── set-dns ─────────────────────────────────────────────────────────────────

func TestSetDNS_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.SetNetworkDNSRequest
	srv.SetNetworkDNSFn = func(_ context.Context, in *weftv1.SetNetworkDNSRequest) (*weftv1.SetNetworkDNSResponse, error) {
		got = in
		return &weftv1.SetNetworkDNSResponse{Network: &weftv1.NetworkInfo{Uuid: in.Uuid, DnsServers: in.DnsServers}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set-dns", "u1", "1.1.1.1,8.8.8.8"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "1.1.1.1,8.8.8.8") {
		t.Errorf("missing dns: %q", out)
	}
	if len(got.DnsServers) != 2 {
		t.Errorf("dns = %v", got.DnsServers)
	}
}

func TestSetDNS_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetNetworkDNSFn = func(_ context.Context, _ *weftv1.SetNetworkDNSRequest) (*weftv1.SetNetworkDNSResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-dns", "u1", "1.1.1.1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── set-sgs ─────────────────────────────────────────────────────────────────

func TestSetSGs_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetNetworkDefaultSecurityGroupsFn = func(_ context.Context, in *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error) {
		return &weftv1.SetNetworkDefaultSecurityGroupsResponse{Network: &weftv1.NetworkInfo{Uuid: in.Uuid, DefaultSecurityGroupUuids: in.SecurityGroupUuids}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set-sgs", "u1", "sg1,sg2"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "sg1,sg2") {
		t.Errorf("missing sgs: %q", out)
	}
}

func TestSetSGs_EmptyList(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetNetworkDefaultSecurityGroupsFn = func(_ context.Context, _ *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error) {
		return &weftv1.SetNetworkDefaultSecurityGroupsResponse{Network: &weftv1.NetworkInfo{Uuid: "u1"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set-sgs", "u1", ""})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "(none)") {
		t.Errorf("missing (none): %q", out)
	}
}

func TestSetSGs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetNetworkDefaultSecurityGroupsFn = func(_ context.Context, _ *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-sgs", "u1", "sg1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── rm ──────────────────────────────────────────────────────────────────────

func TestRm_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "u1"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "u1") {
		t.Errorf("missing uuid: %q", out)
	}
}

func TestRm_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteNetworkFn = func(_ context.Context, _ *weftv1.DeleteNetworkRequest) (*weftv1.DeleteNetworkResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "u1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func TestSplitNonEmpty(t *testing.T) {
	if got := splitNonEmpty("", ","); got != nil {
		t.Errorf("empty -> nil, got %v", got)
	}
	got := splitNonEmpty(" a , b , , c ", ",")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("trim filter = %v", got)
	}
}
