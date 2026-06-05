package sshkey

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
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

// Fixture line — same shape ssh-keygen / pkg/sshkeys emit. The CLI
// only forwards bytes ; the server parses + fingerprints, so this
// doesn't need to be a real (decodable) key.
const fixtureLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleBytes alice@laptop"

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "sshkey" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "add", "import", "rm"}
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

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMSSHKeysFn = func(_ context.Context, _ *weftv1.ListVMSSHKeysRequest) (*weftv1.ListVMSSHKeysResponse, error) {
		return &weftv1.ListVMSSHKeysResponse{Keys: []*weftv1.VMSSHKey{
			{Fingerprint: "SHA256:abc", Type: "ssh-ed25519", Comment: "alice@laptop", AddedAt: "2026-05-29T10:00:00Z"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "web-1"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	for _, want := range []string{"FINGERPRINT", "SHA256:abc", "ssh-ed25519", "alice@laptop"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in :\n%s", want, out)
		}
	}
}

func TestAdd_FromFile(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen *weftv1.AddVMSSHKeyRequest
	srv.AddVMSSHKeyFn = func(_ context.Context, in *weftv1.AddVMSSHKeyRequest) (*weftv1.AddVMSSHKeyResponse, error) {
		seen = in
		return &weftv1.AddVMSSHKeyResponse{Key: &weftv1.VMSSHKey{
			Fingerprint: "SHA256:abc", Type: "ssh-ed25519", Comment: "alice@laptop",
		}}, nil
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pub")
	if err := os.WriteFile(path, []byte(fixtureLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"add", "web-1", "--file", path})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen == nil || seen.VmName != "web-1" || seen.PublicKey != fixtureLine {
		t.Errorf("file path didn't reach server : %+v", seen)
	}
	if !strings.Contains(out, "SHA256:abc") || !strings.Contains(out, "alice@laptop") {
		t.Errorf("expected fingerprint + comment in :\n%s", out)
	}
}

func TestAdd_InlineKey(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen *weftv1.AddVMSSHKeyRequest
	srv.AddVMSSHKeyFn = func(_ context.Context, in *weftv1.AddVMSSHKeyRequest) (*weftv1.AddVMSSHKeyResponse, error) {
		seen = in
		return &weftv1.AddVMSSHKeyResponse{Key: &weftv1.VMSSHKey{Fingerprint: "SHA256:xyz"}}, nil
	}
	captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"add", "web-1", "--key", fixtureLine})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen.PublicKey != fixtureLine {
		t.Errorf("inline key not forwarded : %q", seen.PublicKey)
	}
}

func TestAdd_FileAndKeyMutuallyExclusive(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"add", "web-1", "--file", "a", "--key", "b"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error, got %v", err)
	}
}

func TestAdd_RequiresFileOrKey(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"add", "web-1"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "one of --file or --key") {
		t.Errorf("expected one-of error, got %v", err)
	}
}

func TestImport_MultiLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")
	const body = `# leading comment
ssh-ed25519 AAAA alice
# spacer

ssh-rsa BBBB bob
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := testutil.NewServer(t)
	calls := 0
	srv.AddVMSSHKeyFn = func(_ context.Context, _ *weftv1.AddVMSSHKeyRequest) (*weftv1.AddVMSSHKeyResponse, error) {
		calls++
		return &weftv1.AddVMSSHKeyResponse{Key: &weftv1.VMSSHKey{Fingerprint: "SHA256:f", Type: "ssh-x"}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"import", "web-1", "--file", path})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if calls != 2 {
		t.Errorf("Add calls: got %d, want 2 (skipped comments + blank)", calls)
	}
	if !strings.Contains(out, "added=2") {
		t.Errorf("expected 'added=2' summary in :\n%s", out)
	}
}

func TestImport_RequiresFile(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"import", "web-1"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected missing --file error")
	}
}

func TestRm_PassesFingerprint(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen *weftv1.RemoveVMSSHKeyRequest
	srv.RemoveVMSSHKeyFn = func(_ context.Context, in *weftv1.RemoveVMSSHKeyRequest) (*weftv1.RemoveVMSSHKeyResponse, error) {
		seen = in
		return &weftv1.RemoveVMSSHKeyResponse{}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "web-1", "SHA256:abc"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen == nil || seen.VmName != "web-1" || seen.Fingerprint != "SHA256:abc" {
		t.Errorf("rm did not forward : %+v", seen)
	}
	if !strings.Contains(out, "removed\tweb-1\tSHA256:abc") {
		t.Errorf("expected 'removed' line in :\n%s", out)
	}
}
