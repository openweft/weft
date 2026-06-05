package tenant

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	socket, sshSocket, sshKey := strPtr(""), strPtr(""), strPtr("")
	root := Command(socket, sshSocket, sshKey)
	if root.Use != "tenant" {
		t.Errorf("root.Use : got %q, want tenant", root.Use)
	}
	wantSub := map[string]bool{
		"ls": false, "create <name>": false, "rm <name|uuid>": false,
		"add-admin <name|uuid> <email>":    false,
		"remove-admin <name|uuid> <email>": false,
		"add-member <name|uuid> <email>":   false,
		"remove-member <name|uuid> <email>": false,
	}
	for _, sub := range root.Commands() {
		wantSub[sub.Use] = true
	}
	for use, seen := range wantSub {
		if !seen {
			t.Errorf("missing subcommand : %q", use)
		}
	}
}

func TestLooksLikeUUID(t *testing.T) {
	cases := map[string]bool{
		"e9c3b6c4-9ea2-4f3a-9c1d-2d5a2e3d6b7c": true,
		"E9C3B6C4-9EA2-4F3A-9C1D-2D5A2E3D6B7C": true,
		"too-short": false,
		"e9c3b6c4_9ea2_4f3a_9c1d_2d5a2e3d6b7c": false, // wrong separator
		"e9c3b6c4-9ea2-4f3a-9c1d-2d5a2e3d6b7": false,  // 35 chars
		"":                                            false,
		"acme":                                        false,
	}
	for in, want := range cases {
		if got := looksLikeUUID(in); got != want {
			t.Errorf("looksLikeUUID(%q) : got %v, want %v", in, got, want)
		}
	}
}

func TestAddMemberCmd_GroupsFlagRepeatable(t *testing.T) {
	socket, sshSocket, sshKey := strPtr(""), strPtr(""), strPtr("")
	cmd := addMemberCmd(socket, sshSocket, sshKey)
	flag := cmd.Flags().Lookup("group")
	if flag == nil {
		t.Fatal("--group flag missing")
	}
	if !strings.Contains(flag.Usage, "Repeatable") {
		t.Errorf("--group help should mention repeatable, got %q", flag.Usage)
	}
}

func TestCreateCmd_DomainFlag(t *testing.T) {
	socket, sshSocket, sshKey := strPtr(""), strPtr(""), strPtr("")
	cmd := createCmd(socket, sshSocket, sshKey)
	flag := cmd.Flags().Lookup("domain")
	if flag == nil {
		t.Fatal("--domain flag missing")
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	// Every verb should propagate the shared.Client dial failure
	// cleanly rather than panic. We point at a socket that can't
	// exist so every command fails fast. Mirrors the host package's
	// dial-error harness — execute the ROOT command with the verb
	// as the first arg so cobra parses subcommand + positional args
	// the same way the operator's shell does.
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"create", []string{"create", "acme"}},
		{"rm", []string{"rm", "acme"}},
		{"add-admin", []string{"add-admin", "acme", "alice@acme.org"}},
		{"remove-admin", []string{"remove-admin", "acme", "alice@acme.org"}},
		{"add-member", []string{"add-member", "acme", "alice@acme.org"}},
		{"remove-member", []string{"remove-member", "acme", "alice@acme.org"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-tenant-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error, got nil", c.name)
			}
		})
	}
}

// cobra import used for the SetArgs reset above.
var _ = cobra.NoArgs
