package dnsrecord

import (
	"testing"

	"github.com/spf13/cobra"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "dns-record" {
		t.Errorf("root.Use : got %q, want dns-record", root.Use)
	}
	want := map[string]bool{
		"ls --zone=<uuid>": false,
		"create --zone=<uuid> --name=<...> --type=<...> --value=<...>": false,
		"update <uuid>": false,
		"rm <uuid>":     false,
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

func TestCreateCmd_OptionalFlags(t *testing.T) {
	cmd := createCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"zone", "name", "type", "value", "ttl", "priority"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestUpdateCmd_OptionalFlags(t *testing.T) {
	cmd := updateCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"value", "ttl", "priority"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestLsCmd_OptionalFlags(t *testing.T) {
	cmd := lsCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"zone", "format"} {
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
		{"ls", []string{"ls", "--zone", "11111111-2222-3333-4444-555555555555"}},
		{"create", []string{"create", "--zone", "11111111-2222-3333-4444-555555555555", "--name", "www", "--type", "A", "--value", "10.0.0.1"}},
		{"update", []string{"update", "11111111-2222-3333-4444-555555555555", "--value", "10.0.0.2"}},
		{"rm", []string{"rm", "11111111-2222-3333-4444-555555555555"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-dnsrecord-"+c.name+".sock"), strPtr(""), strPtr(""))
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
