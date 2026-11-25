package secretsource

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/go-freedesktop/secretservice"
	"github.com/go-macos/keychain"
)

// The managers, replaceable by tests.
var (
	keychainGet      = keychain.Get
	secretserviceGet = secretservice.Get
	// secretserviceGetAttrs reads an item by arbitrary attributes.
	secretserviceGetAttrs = secretservice.GetByAttributes
)

// Security framework statuses that mean "the keychain will not answer now".
const (
	errSecAuthFailed          = -25293
	errSecInteractionNotAllow = -25308
	errSecUserCanceled        = -128
)

func isSSNotFound(err error) bool { return errors.Is(err, secretservice.ErrNotFound) }

func mapKeychain(r Ref, err error) error {
	var ke *keychain.Error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnavailable):
		return err
	case errors.Is(err, keychain.ErrNotFound):
		return fmt.Errorf("%w: keychain item %s/%s (add it with `security add-generic-password -s SERVICE -a ACCOUNT -w`)", ErrNotFound, r.Service, r.Account)
	case errors.Is(err, keychain.ErrUnsupported):
		return fmt.Errorf("%w: the Keychain exists on macOS only", ErrUnavailable)
	case errors.As(err, &ke) && (ke.Status == errSecInteractionNotAllow || ke.Status == errSecAuthFailed || ke.Status == errSecUserCanceled):
		return fmt.Errorf("%w: the Keychain is locked or the access was refused (OSStatus %d)", ErrUnavailable, ke.Status)
	}
	return fmt.Errorf("secretsource: %s: %w", r, err)
}

func mapSecretService(r Ref, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnavailable):
		return err
	case errors.Is(err, secretservice.ErrNotFound):
		if r.Attrs != "" {
			return fmt.Errorf("%w: Secret Service item with %s", ErrNotFound, r.Attrs)
		}
		return fmt.Errorf("%w: Secret Service item service=%s account=%s", ErrNotFound, r.Service, r.Account)
	case errors.Is(err, secretservice.ErrAmbiguous):
		return fmt.Errorf("%w: several Secret Service items match %s; add an attribute to tell them apart", ErrAmbiguous, r)
	case errors.Is(err, secretservice.ErrUnavailable):
		return fmt.Errorf("%w: no Secret Service on the session bus (start GNOME Keyring or enable KeePassXC's Secret Service Integration): %v", ErrUnavailable, err)
	}
	// An unlock the person dismissed, or a daemon error.
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, r, err)
}

// readFile reads a secret from a regular file that its owner alone can read or
// write, and strips one trailing newline. The checks are made on the open
// descriptor, not the path, so the file cannot be swapped between check and
// read.
func readFile(path string) ([]byte, error) {
	return readFileVia(path, openSecretFile, checkSecretFile, true)
}

// readFileVia is readFile with the open and permission steps chosen by the
// caller, and the trailing newline stripped only when trim is set.
func readFileVia(path string, open func(string) (*os.File, error), check func(*os.File, fs.FileInfo, string) error, trim bool) ([]byte, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s: not a regular file", path)
	}
	if err == nil {
		err = check(f, st, path)
	}
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err == nil && len(b) > MaxSize {
		err = fmt.Errorf("%w: %s is longer than %d bytes", ErrTooLarge, path, MaxSize)
	}
	if err != nil {
		clear(b)
		return nil, err
	}
	n := len(b)
	if trim && n > 0 && b[n-1] == '\n' {
		n--
		if n > 0 && b[n-1] == '\r' {
			n--
		}
	}
	if n == 0 {
		// A truncated or half-written file must not become an empty PIN or
		// password; the other backends refuse an empty entry the same way.
		return nil, fmt.Errorf("%w: %s", ErrEmpty, path)
	}
	return b[:n], nil
}
