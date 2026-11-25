package secretsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// maxCredName is NAME_MAX: a credential name is "suitable for inclusion in a
// filename" (systemd.io/CREDENTIALS).
const maxCredName = 255

// checkCredName refuses a name that is not a plain file name: empty, "." or
// "..", longer than a file name, or holding "/", NUL or a control character.
// The name is joined to $CREDENTIALS_DIRECTORY and handed to systemd-creds, so
// it must be unable to leave the directory or look like anything else.
func checkCredName(name string) error {
	switch {
	case name == "":
		return errors.New("empty credential name")
	case name == "." || name == "..":
		return errors.New("credential name is a directory entry")
	case strings.HasSuffix(name, ".cred"):
		return errors.New("write the credential name, without the .cred suffix of its file")
	case len(name) > maxCredName:
		return fmt.Errorf("credential name longer than %d bytes", maxCredName)
	case strings.ContainsRune(name, '/'):
		return errors.New("credential name holds a slash")
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return errors.New("credential name holds a control character")
	}
	return nil
}

// Seams, replaced by tests.
var (
	// goos and getenv are shared with keepassxc.go.
	geteuid      = os.Geteuid
	runCreds     = execCreds
	credsBin     = "/usr/bin/systemd-creds"
	credstoreEnc = []string{"/etc/credstore.encrypted", "/run/credstore.encrypted", "/usr/lib/credstore.encrypted"}
)

// openSystemdCreds resolves a systemd credential. The credential the service
// manager placed in $CREDENTIALS_DIRECTORY (LoadCredential=,
// LoadCredentialEncrypted=, SetCredentialEncrypted=) wins; when the variable is
// unset or the directory lacks the name, the encrypted credential is decrypted
// from the credstore.encrypted directories by systemd-creds. A credential that
// is present but unsafe (symlink, not a regular file, group or other access) is
// refused, never skipped. The bytes are returned exactly as stored: no
// trimming of a trailing newline, no text assumption (unlike file:, which
// strips one).
func openSystemdCreds(ctx context.Context, name string) ([]byte, error) {
	if goos != "linux" {
		return nil, fmt.Errorf("%w: systemd credentials exist on Linux only (this is %s)", ErrUnavailable, goos)
	}
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("%w: $CREDENTIALS_DIRECTORY is not an absolute path", ErrUnavailable)
		}
		if st, err := os.Stat(dir); err == nil && st.Mode().Perm()&0o007 != 0 {
			return nil, fmt.Errorf("%w: %s is accessible to others (mode %04o)", ErrUnavailable, dir, st.Mode().Perm())
		}
		b, err := readFileVia(filepath.Join(dir, name), openNoFollow, checkCredFile, false)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			return b, err
		}
	}
	return decryptCred(ctx, name)
}

// decryptCred finds NAME or NAME.cred in the credstore.encrypted directories
// and decrypts it with `systemd-creds decrypt --name=NAME PATH -`: the secret
// comes back on stdout only.
func decryptCred(ctx context.Context, name string) ([]byte, error) {
	path := ""
	for _, dir := range credstoreEnc {
		for _, file := range []string{name, name + ".cred"} {
			p := filepath.Join(dir, file)
			if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
				path = p
				break
			}
		}
		if path != "" {
			break
		}
	}
	if path == "" {
		return nil, fmt.Errorf("%w: credential %q is neither in $CREDENTIALS_DIRECTORY nor readable in a credstore.encrypted directory", ErrNotFound, name)
	}
	// systemd 256 added --user: a user-scoped credential only decrypts in the
	// user's context. A non-root caller tries it first, then system scope.
	scopes := []string{""}
	if geteuid() != 0 {
		scopes = []string{"--user", ""}
	}
	var last error
	for _, scope := range scopes {
		args := []string{"decrypt", "--name=" + name}
		if scope != "" {
			args = append(args, scope)
		}
		args = append(args, path, "-")
		b, stderr, err := runCreds(ctx, args)
		if err == nil {
			return b, nil
		}
		clear(b)
		if errors.Is(err, ErrTooLarge) || ctx.Err() != nil {
			return nil, err
		}
		// The child's stderr is systemd-creds' own diagnostic from a decrypt
		// that produced nothing; it never holds the secret.
		last = fmt.Errorf("%w: systemd-creds decrypt %s: %v: %s; the credential may need re-sealing (the TPM2 PCR policy or the host key it was sealed to may have changed): run `systemd-creds encrypt` again from a copy of the secret", ErrUnavailable, path, err, clip(strings.TrimSpace(stderr)))
	}
	return nil, last
}

// tail keeps the first bytes of a child's stderr and discards the rest.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	if room := 512 - len(t.b); room > 0 {
		t.b = append(t.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// execCreds runs systemd-creds with no shell, no stdin and an empty
// environment, and reads at most MaxSize bytes of its stdout; a child that
// writes more is killed. It returns the child's stdout and a clipped stderr.
func execCreds(ctx context.Context, args []string) ([]byte, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, credsBin, args...)
	cmd.Env = []string{}
	// A grandchild holding the pipe open must not outlive the kill.
	cmd.WaitDelay = 2 * time.Second
	var se tail
	so := &capped{cancel: cancel, b: make([]byte, 0, MaxSize)} // never reallocated, so never copied unwiped
	cmd.Stderr = &se
	cmd.Stdout = so
	err := cmd.Run()
	if so.over {
		clear(so.b)
		return nil, string(se.b), fmt.Errorf("%w: systemd-creds wrote more than %d bytes", ErrTooLarge, MaxSize)
	}
	if err != nil {
		clear(so.b)
		if cerr := ctx.Err(); cerr != nil {
			return nil, string(se.b), fmt.Errorf("%w: gave up waiting for systemd-creds: %v", ErrUnavailable, cerr)
		}
		return nil, string(se.b), err
	}
	return so.b, string(se.b), nil
}

// capped collects at most MaxSize bytes; one more kills the child.
type capped struct {
	b      []byte
	over   bool
	cancel context.CancelFunc
}

func (c *capped) Write(p []byte) (int, error) {
	if len(c.b)+len(p) > MaxSize {
		c.over = true
		c.cancel()
		return 0, io.ErrShortWrite
	}
	c.b = append(c.b, p...)
	return len(p), nil
}
