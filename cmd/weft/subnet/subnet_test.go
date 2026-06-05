package subnet

import (
	"testing"

	"github.com/spf13/cobra"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "subnet" {
		t.Errorf("root.Use : got %q, want subnet", root.Use)
	}
	want := map[string]bool{
		"ls":                              false,
		"show <uuid>":                     false,
		"create --network=<uuid> --cidr=<...>": false,
		"update <uuid>":                   false,
		"rm <uuid>":                       false,
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
	for _, f := range []string{"network", "cidr", "gateway", "name", "description", "dns"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestUpdateCmd_OptionalFlags(t *testing.T) {
	cmd := updateCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"name", "description", "gateway", "dns", "clear-dns"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestLsCmd_OptionalFlags(t *testing.T) {
	cmd := lsCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"network", "project", "format"} {
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
		{"show", []string{"show", "11111111-2222-3333-4444-555555555555"}},
		{"create", []string{"create", "--network", "11111111-2222-3333-4444-555555555555", "--cidr", "10.0.0.0/24"}},
		{"update", []string{"update", "11111111-2222-3333-4444-555555555555", "--name", "x"}},
		{"rm", []string{"rm", "11111111-2222-3333-4444-555555555555"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-subnet-"+c.name+".sock"), strPtr(""), strPtr(""))
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
