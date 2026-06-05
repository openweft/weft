package schedulingrule

import (
	"context"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "scheduling-rule" {
		t.Errorf("root.Use : got %q", root.Use)
	}
	want := map[string]bool{
		"ls": false, "create": false, "update <uuid>": false, "rm <uuid>": false,
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

func TestLs_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListSchedulingRulesFn = func(_ context.Context, _ *weftv1.ListSchedulingRulesRequest) (*weftv1.ListSchedulingRulesResponse, error) {
		return &weftv1.ListSchedulingRulesResponse{Rules: []*weftv1.SchedulingRuleInfo{{Uuid: "u1", Name: "n1", TargetCount: 3}}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ls: %v", err)
	}
}

func TestCreate_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateSchedulingRuleFn = func(_ context.Context, req *weftv1.CreateSchedulingRuleRequest) (*weftv1.CreateSchedulingRuleResponse, error) {
		if req.Name != "r1" || req.Selector != "az=DC-A" || req.TargetCount != 3 {
			t.Errorf("req: %+v", req)
		}
		return &weftv1.CreateSchedulingRuleResponse{Rule: &weftv1.SchedulingRuleInfo{Uuid: "u1", Name: req.Name, TargetCount: req.TargetCount}, Created: true}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"create", "--name", "r1", "--selector", "az=DC-A", "--target-count", "3"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestUpdate_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.UpdateSchedulingRuleFn = func(_ context.Context, req *weftv1.UpdateSchedulingRuleRequest) (*weftv1.UpdateSchedulingRuleResponse, error) {
		if req.Uuid != "u1" || req.TargetCount != 5 {
			t.Errorf("req: %+v", req)
		}
		return &weftv1.UpdateSchedulingRuleResponse{Rule: &weftv1.SchedulingRuleInfo{Uuid: req.Uuid, TargetCount: req.TargetCount}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"update", "u1", "--target-count", "5"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestRm_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.DeleteSchedulingRuleFn = func(_ context.Context, req *weftv1.DeleteSchedulingRuleRequest) (*weftv1.DeleteSchedulingRuleResponse, error) {
		if req.Uuid != "u1" {
			t.Errorf("uuid: %q", req.Uuid)
		}
		return &weftv1.DeleteSchedulingRuleResponse{DeletedUuid: req.Uuid}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "u1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rm: %v", err)
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"create", []string{"create", "--name", "r"}},
		{"update", []string{"update", "u1"}},
		{"rm", []string{"rm", "u1"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-sr-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error", c.name)
			}
		})
	}
}
