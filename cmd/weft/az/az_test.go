package az

import (
	"testing"

	"github.com/spf13/cobra"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "az" {
		t.Errorf("root.Use : got %q, want az", root.Use)
	}
	want := map[string]bool{
		"ls": false, "show <code|uuid>": false,
		"create <code>": false, "update <code|uuid>": false,
		"rm <code|uuid>": false,
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

func TestLooksLikeUUID(t *testing.T) {
	cases := map[string]bool{
		"e9c3b6c4-9ea2-4f3a-9c1d-2d5a2e3d6b7c": true,
		"E9C3B6C4-9EA2-4F3A-9C1D-2D5A2E3D6B7C": true,
		"DC-A":   false,
		"":       false,
		"toolong-e9c3b6c4-9ea2-4f3a-9c1d-2d5a": false,
	}
	for in, want := range cases {
		if got := LooksLikeUUID(in); got != want {
			t.Errorf("LooksLikeUUID(%q) : got %v, want %v", in, got, want)
		}
	}
}

func TestCreateCmd_OptionalFlags(t *testing.T) {
	cmd := createCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"name", "region", "status"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s flag missing", f)
		}
	}
}

func TestUpdateCmd_OptionalFlags(t *testing.T) {
	cmd := updateCmd(strPtr(""), strPtr(""), strPtr(""))
	for _, f := range []string{"name", "region", "status"} {
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
		{"show", []string{"show", "DC-A"}},
		{"create", []string{"create", "DC-A", "--name", "x"}},
		{"update", []string{"update", "DC-A", "--name", "x"}},
		{"rm", []string{"rm", "DC-A"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-az-"+c.name+".sock"), strPtr(""), strPtr(""))
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
