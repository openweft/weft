package floatingip

import (
	"testing"

	"github.com/spf13/cobra"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "floating-ip" {
		t.Errorf("root.Use : got %q, want floating-ip", root.Use)
	}
	want := map[string]bool{
		"ls":              false,
		"show <uuid>":     false,
		"allocate":        false,
		"release <uuid>":  false,
		"map <uuid>":      false,
		"unmap <uuid>":    false,
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
