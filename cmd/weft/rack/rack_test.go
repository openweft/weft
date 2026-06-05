package rack

import (
	"testing"

	"github.com/spf13/cobra"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "rack" {
		t.Errorf("root.Use : got %q, want rack", root.Use)
	}
	want := map[string]bool{
		"ls":            false,
		"show <uuid>":   false,
		"create <code>": false,
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

func TestCreateCmd_AZFlagRequired(t *testing.T) {
	cmd := createCmd(strPtr("/tmp/nope.sock"), strPtr(""), strPtr(""))
	// Execute without --az ; must surface an error before any dial.
	cmd.SetArgs([]string{"R1"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("create without --az should error")
	}
}

func TestCreateCmd_FlagsRegistered(t *testing.T) {
	cmd := createCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"az", "name", "status", "height-u"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestUpdateCmd_HeightUDefaultsToMinusOne(t *testing.T) {
	cmd := updateCmd(strPtr(""), strPtr(""), strPtr(""))
	flag := cmd.Flags().Lookup("height-u")
	if flag == nil {
		t.Fatal("--height-u missing")
	}
	if flag.DefValue != "-1" {
		t.Errorf("--height-u default : got %q, want -1 (sentinel keep-current)", flag.DefValue)
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"show", []string{"show", "u-1"}},
		{"create", []string{"create", "R1", "--az", "DC-A"}},
		{"update", []string{"update", "u-1", "--name", "x"}},
		{"rm", []string{"rm", "u-1"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-rack-"+c.name+".sock"), strPtr(""), strPtr(""))
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
