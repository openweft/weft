package loadbalancer

import (
	"testing"

	"github.com/spf13/cobra"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "loadbalancer" {
		t.Errorf("root.Use : got %q, want loadbalancer", root.Use)
	}
	want := map[string]bool{
		"ls":                  false,
		"show <uuid>":         false,
		"create":              false,
		"update <uuid>":       false,
		"set-backends <uuid>": false,
		"rm <uuid>":           false,
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

func TestCommand_Aliases(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	found := false
	for _, a := range root.Aliases {
		if a == "lb" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected alias %q on root", "lb")
	}
}

func TestCreateCmd_OptionalFlags(t *testing.T) {
	cmd := createCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"project", "name", "listen", "protocol", "backends"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestUpdateCmd_OptionalFlags(t *testing.T) {
	cmd := updateCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"name", "listen", "protocol"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestSetBackendsCmd_OptionalFlags(t *testing.T) {
	cmd := setBackendsCmd(strPtr(""), strPtr(""), strPtr(""))
	if cmd.Flags().Lookup("backends") == nil {
		t.Errorf("--backends flag missing")
	}
}

func TestParseBackends(t *testing.T) {
	if out, err := parseBackends(""); err != nil || out != nil {
		t.Errorf("empty : got (%v, %v), want (nil, nil)", out, err)
	}
	out, err := parseBackends("10.0.0.1:80@5,10.0.0.2:80")
	if err != nil {
		t.Fatalf("parse : %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len : got %d, want 2", len(out))
	}
	if out[0].Address != "10.0.0.1:80" || out[0].Weight != 5 {
		t.Errorf("backend[0] : got %+v", out[0])
	}
	if out[1].Address != "10.0.0.2:80" || out[1].Weight != 1 {
		t.Errorf("backend[1] : got %+v", out[1])
	}
	if _, err := parseBackends("10.0.0.1:80@bogus"); err == nil {
		t.Errorf("expected error on bad weight")
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"show", []string{"show", "11111111-2222-3333-4444-555555555555"}},
		{"create", []string{"create", "--project", "default", "--name", "lb1", "--listen", "10.0.0.1:80", "--protocol", "l4_tcp"}},
		{"update", []string{"update", "11111111-2222-3333-4444-555555555555", "--name", "x"}},
		{"set-backends", []string{"set-backends", "11111111-2222-3333-4444-555555555555", "--backends", "10.0.0.2:80"}},
		{"rm", []string{"rm", "11111111-2222-3333-4444-555555555555"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-lb-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error", c.name)
			}
		})
	}
}

var _ = cobra.NoArgs
