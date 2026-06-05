package sshkeycatalogue

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	root := Command(strPtr(""), strPtr(""), strPtr(""))
	if root.Use != "sshkey-catalogue" {
		t.Errorf("root.Use : got %q", root.Use)
	}
	want := map[string]bool{
		"ls": false, "add": false, "rm <uuid|name>": false, "import": false,
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
	srv.ListSSHKeyCatalogueFn = func(_ context.Context, _ *weftv1.ListSSHKeyCatalogueRequest) (*weftv1.ListSSHKeyCatalogueResponse, error) {
		return &weftv1.ListSSHKeyCatalogueResponse{Keys: []*weftv1.SSHKeyCatalogueEntry{{Uuid: "u1", Name: "alice", Fingerprint: "SHA256:abc"}}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ls: %v", err)
	}
}

func TestAdd_Success(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.AddSSHKeyCatalogueFn = func(_ context.Context, req *weftv1.AddSSHKeyCatalogueRequest) (*weftv1.AddSSHKeyCatalogueResponse, error) {
		if req.Name != "alice" || req.PublicKey == "" {
			t.Errorf("req: %+v", req)
		}
		return &weftv1.AddSSHKeyCatalogueResponse{Key: &weftv1.SSHKeyCatalogueEntry{Uuid: "u1", Name: req.Name}, Added: true}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"add", "--name", "alice", "--public-key", "ssh-ed25519 AAAA test"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("add: %v", err)
	}
}

func TestRm_ByName(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RemoveSSHKeyCatalogueFn = func(_ context.Context, req *weftv1.RemoveSSHKeyCatalogueRequest) (*weftv1.RemoveSSHKeyCatalogueResponse, error) {
		if req.Name != "alice" {
			t.Errorf("name: %q", req.Name)
		}
		return &weftv1.RemoveSSHKeyCatalogueResponse{DeletedUuid: "u1"}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"rm", "alice"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rm: %v", err)
	}
}

func TestImport_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(path, []byte("ssh-ed25519 AAAA k1\nssh-ed25519 AAAA k2\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	srv := testutil.NewServer(t)
	srv.ImportSSHKeyCatalogueFn = func(_ context.Context, req *weftv1.ImportSSHKeyCatalogueRequest) (*weftv1.ImportSSHKeyCatalogueResponse, error) {
		if req.NamePrefix != "team" || req.Blob == "" {
			t.Errorf("req: %+v", req)
		}
		return &weftv1.ImportSSHKeyCatalogueResponse{Imported: []*weftv1.SSHKeyCatalogueEntry{{Uuid: "u1", Name: "team-1"}}}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"import", "--name-prefix", "team", "--file", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import: %v", err)
	}
}

func TestLooksLikeUUID(t *testing.T) {
	if !looksLikeUUID("e9c3b6c4-9ea2-4f3a-9c1d-2d5a2e3d6b7c") {
		t.Error("expected UUID")
	}
	if looksLikeUUID("alice") {
		t.Error("expected non-UUID")
	}
}

func TestAllSubcommands_DialError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"ls", []string{"ls"}},
		{"add", []string{"add", "--name", "n", "--public-key", "k"}},
		{"rm", []string{"rm", "alice"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := Command(strPtr("/tmp/nope-sk-"+c.name+".sock"), strPtr(""), strPtr(""))
			cmd.SetArgs(c.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s : expected dial error", c.name)
			}
		})
	}
}
