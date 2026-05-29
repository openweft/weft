package securitygroup

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

// writeRulesFile creates a small HCL fixture so create / set-rules
// can decode it. Returns the path on disk.
func writeRulesFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.hcl")
	body := `
rule {
  direction   = "ingress"
  protocol    = "tcp"
  port_min    = 22
  port_max    = 22
  remote_cidr = "10.0.0.0/8"
}
rule {
  direction         = "egress"
  protocol          = "udp"
  remote_group_uuid = "sg-other"
}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	return p
}

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "security-group" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "create", "rename", "set-description", "set-rules", "rm"}
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

// ── helpers ─────────────────────────────────────────────────────────────────

func TestLoadRulesFile_Empty(t *testing.T) {
	if rules, err := loadRulesFile(""); err != nil || rules != nil {
		t.Errorf("empty path expected nil/nil, got %v err=%v", rules, err)
	}
}

func TestLoadRulesFile_BadPath(t *testing.T) {
	if _, err := loadRulesFile("/nope/abs/path.hcl"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestLoadRulesFile_Valid(t *testing.T) {
	p := writeRulesFile(t)
	rules, err := loadRulesFile(p)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("len = %d", len(rules))
	}
	if rules[0].Direction != "ingress" || rules[0].Protocol != "tcp" || rules[0].PortMin != 22 {
		t.Errorf("rule 0 = %+v", rules[0])
	}
	if rules[1].RemoteGroupUuid != "sg-other" {
		t.Errorf("rule 1 RemoteGroupUuid = %q", rules[1].RemoteGroupUuid)
	}
}

// ── ls ──────────────────────────────────────────────────────────────────────

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListSecurityGroupsFn = func(_ context.Context, _ *vzdv1.ListSecurityGroupsRequest) (*vzdv1.ListSecurityGroupsResponse, error) {
		return &vzdv1.ListSecurityGroupsResponse{Groups: []*vzdv1.SecurityGroupInfo{
			{Uuid: "u1", ProjectUuid: "p1", Name: "ssh", Description: "ssh in", Rules: []*vzdv1.SecurityRule{{Direction: "ingress", Protocol: "tcp", PortMin: 22, PortMax: 22, RemoteCidr: "0/0"}}, CreatedAtUnixNs: 1700000000000000000},
			{Uuid: "u2", ProjectUuid: "p2", Name: "noop"}, // empty desc → dash
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--project", "p"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "ssh") || !strings.Contains(out, "noop") {
		t.Errorf("missing rows: %q", out)
	}
}

func TestLs_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListSecurityGroupsFn = func(_ context.Context, _ *vzdv1.ListSecurityGroupsRequest) (*vzdv1.ListSecurityGroupsResponse, error) {
		return &vzdv1.ListSecurityGroupsResponse{Groups: []*vzdv1.SecurityGroupInfo{
			{Uuid: "u1", Name: "ssh", Rules: []*vzdv1.SecurityRule{{Direction: "ingress", Protocol: "tcp"}}},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, `"name": "ssh"`) {
		t.Errorf("missing name: %q", out)
	}
}

func TestLs_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListSecurityGroupsFn = func(_ context.Context, _ *vzdv1.ListSecurityGroupsRequest) (*vzdv1.ListSecurityGroupsResponse, error) {
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
	var got *vzdv1.CreateSecurityGroupRequest
	srv.CreateSecurityGroupFn = func(_ context.Context, in *vzdv1.CreateSecurityGroupRequest) (*vzdv1.CreateSecurityGroupResponse, error) {
		got = in
		return &vzdv1.CreateSecurityGroupResponse{Group: &vzdv1.SecurityGroupInfo{Uuid: "u1", Name: in.Name, Rules: in.Rules}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"create", "--project", "p", "--name", "ssh", "--description", "ssh in"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if got.Name != "ssh" || got.Description != "ssh in" {
		t.Errorf("got = %+v", got)
	}
	if !strings.Contains(out, "rules=0") {
		t.Errorf("missing rules count: %q", out)
	}
}

func TestCreate_WithRulesFile(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateSecurityGroupFn = func(_ context.Context, in *vzdv1.CreateSecurityGroupRequest) (*vzdv1.CreateSecurityGroupResponse, error) {
		if len(in.Rules) != 2 {
			t.Errorf("expected 2 rules, got %d", len(in.Rules))
		}
		return &vzdv1.CreateSecurityGroupResponse{Group: &vzdv1.SecurityGroupInfo{Uuid: "u1", Name: in.Name, Rules: in.Rules}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "--name", "ssh", "--rules-file", writeRulesFile(t)})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestCreate_LoadRulesError(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "--name", "ssh", "--rules-file", "/bad/path.hcl"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected hcl decode error")
	}
}

func TestCreate_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateSecurityGroupFn = func(_ context.Context, _ *vzdv1.CreateSecurityGroupRequest) (*vzdv1.CreateSecurityGroupResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "--name", "ssh"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── rename ──────────────────────────────────────────────────────────────────

func TestRename_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenameSecurityGroupFn = func(_ context.Context, in *vzdv1.RenameSecurityGroupRequest) (*vzdv1.RenameSecurityGroupResponse, error) {
		return &vzdv1.RenameSecurityGroupResponse{Group: &vzdv1.SecurityGroupInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "u1", "new"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestRename_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RenameSecurityGroupFn = func(_ context.Context, _ *vzdv1.RenameSecurityGroupRequest) (*vzdv1.RenameSecurityGroupResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rename", "u1", "new"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── set-description ─────────────────────────────────────────────────────────

func TestSetDescription_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetSecurityGroupDescriptionFn = func(_ context.Context, in *vzdv1.SetSecurityGroupDescriptionRequest) (*vzdv1.SetSecurityGroupDescriptionResponse, error) {
		return &vzdv1.SetSecurityGroupDescriptionResponse{Group: &vzdv1.SecurityGroupInfo{Uuid: in.Uuid, Description: in.Description}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-description", "u1", "new desc"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestSetDescription_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetSecurityGroupDescriptionFn = func(_ context.Context, _ *vzdv1.SetSecurityGroupDescriptionRequest) (*vzdv1.SetSecurityGroupDescriptionResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-description", "u1", "d"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error")
	}
}

// ── set-rules ───────────────────────────────────────────────────────────────

func TestSetRules_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetSecurityGroupRulesFn = func(_ context.Context, in *vzdv1.SetSecurityGroupRulesRequest) (*vzdv1.SetSecurityGroupRulesResponse, error) {
		return &vzdv1.SetSecurityGroupRulesResponse{Group: &vzdv1.SecurityGroupInfo{Uuid: in.Uuid, Rules: in.Rules}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-rules", "u1", "--rules-file", writeRulesFile(t)})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestSetRules_LoadError(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-rules", "u1", "--rules-file", "/bad/abs/path.hcl"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected hcl decode error")
	}
}

func TestSetRules_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.SetSecurityGroupRulesFn = func(_ context.Context, _ *vzdv1.SetSecurityGroupRulesRequest) (*vzdv1.SetSecurityGroupRulesResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set-rules", "u1", "--rules-file", writeRulesFile(t)})
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
		t.Errorf("missing echo: %q", out)
	}
}

func TestRm_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteSecurityGroupFn = func(_ context.Context, _ *vzdv1.DeleteSecurityGroupRequest) (*vzdv1.DeleteSecurityGroupResponse, error) {
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
	rules := writeRulesFile(t)
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"create", []string{"create", "--name", "n"}},
		{"rename", []string{"rename", "u1", "new"}},
		{"set-description", []string{"set-description", "u1", "d"}},
		{"set-rules", []string{"set-rules", "u1", "--rules-file", rules}},
		{"rm", []string{"rm", "u1"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-sg-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s: expected dial error", c.name)
			}
		})
	}
}
