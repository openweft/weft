package secretsource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/go-freedesktop/secretservice"
	"github.com/go-macos/keychain"
)

// ErrNoDelete means the reference cannot be deleted by this package: a file or
// a systemd credential is never managed here, and a Secret Service lookup by
// arbitrary attributes names no single item to remove.
var ErrNoDelete = errors.New("secretsource: cannot delete this reference")

// The managers' delete calls, replaceable by tests.
var (
	keychainDelete = keychain.Delete
	ssDelete       = secretservice.Delete
)

// The attributes the Secret Service backend files an item under
// (secretservice v0.4.0 backend_linux.go: attrServiceKey, attrAccountKey).
const (
	ssServiceKey = "service"
	ssAccountKey = "account"
)

// Delete removes the entry ref names and never prints it:
//
//	keychain:SERVICE/ACCOUNT        the Keychain generic-password item
//	secretservice:SERVICE/ACCOUNT   the Secret Service item
//	keepassxc:URL                   the KeePassXC entry whose URL field is URL
//
// An absent entry is an error that is both [ErrNotFound] and
// [fs.ErrNotExist], so a caller that wants "already gone" to succeed says so
// with errors.Is. Several matching entries are [ErrAmbiguous] and nothing is
// deleted. file:, systemd-creds: and a Secret Service lookup by attributes are
// [ErrNoDelete].
//
// The managers have no "does it exist" call, so the entry is looked up first
// the way [Store] does before it replaces one: the value may transit memory
// (a keychain or Secret Service item) and is wiped at once; a KeePassXC reply
// is decoded without its password field. Nothing is returned or logged.
//
// KeePassXC deletes with its browser protocol's delete-entry
// (BrowserAction.cpp handleDeleteEntry, BrowserService.cpp deleteEntry in
// 2.8.0): KeePassXC asks the person to confirm, then MOVES the entry to the
// database's Recycle Bin. The password stays in the database until the Recycle
// Bin is emptied.
func Delete(ctx context.Context, ref string) error {
	r, err := Parse(ref)
	if err != nil {
		return err
	}
	switch {
	case r.Scheme == "file":
		return fmt.Errorf("%w: file: is never managed by this package", ErrNoDelete)
	case r.Scheme == "systemd-creds":
		return fmt.Errorf("%w: a systemd credential is not managed by this package", ErrNoDelete)
	case r.Scheme == "secretservice" && r.Attrs != "":
		return fmt.Errorf("%w: a Secret Service lookup by attributes names no single item; use secretservice:SERVICE/ACCOUNT", ErrNoDelete)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch r.Scheme {
	case "keepassxc":
		return deleteKeePassXC(ctx, r)
	case "keychain":
		// The seam is read here, not inside the goroutine call starts: a call
		// abandoned at the deadline must not read a variable the caller has
		// moved on from.
		get := keychainGet
		return deleteKV(ctx, r,
			func() ([]byte, error) { return get(r.Service, r.Account) },
			keychainDelete, isKeychainNotFound, mapKeychain)
	}
	getAttrs := secretserviceGetAttrs
	return deleteKV(ctx, r,
		func() ([]byte, error) {
			return getAttrs(map[string]string{ssServiceKey: r.Service, ssAccountKey: r.Account})
		},
		ssDelete, isSSNotFound, mapSecretService)
}

func isKeychainNotFound(err error) bool { return errors.Is(err, keychain.ErrNotFound) }

// notThere is the error for an entry that is absent: ErrNotFound, and
// fs.ErrNotExist for a caller that treats "already gone" as success.
func notThere(r Ref) error {
	return fmt.Errorf("%w (%w): nothing to delete at %s", ErrNotFound, fs.ErrNotExist, r)
}

// deleteKV deletes a keychain or Secret Service item after finding it: probe
// is the lookup (whose value is wiped at once), del the manager's delete.
func deleteKV(ctx context.Context, r Ref, probe func() ([]byte, error), del func(string, string) error,
	notFound func(error) bool, mapErr func(Ref, error) error) error {
	b, err := call(ctx, probe)
	switch {
	case err == nil:
		clear(b)
	case notFound(err):
		return notThere(r)
	default:
		return mapErr(r, err)
	}
	if _, err := call(ctx, func() ([]byte, error) { return nil, del(r.Service, r.Account) }); err != nil {
		return mapErr(r, err)
	}
	return nil
}

// deleteKeePassXC finds the one entry whose URL is r.Entry (by URL, never by
// uuid or path: those crash KeePassXC 2.8.0 on an entry without a URL, and
// Parse refuses them) and
// asks KeePassXC to delete it by the uuid it reported.
func deleteKeePassXC(ctx context.Context, r Ref) error {
	s, err := openSession(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	// The reply is decoded without its password field.
	var found struct {
		Entries []struct {
			Name  string `json:"name"`
			UUID  string `json:"uuid"`
			Group string `json:"group"`
		} `json:"entries"`
	}
	if err := s.k.call("get-logins", map[string]any{"url": r.Entry, "keys": s.keys}, &found); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notThere(r)
		}
		return waiting(err)
	}
	es := found.Entries
	switch len(es) {
	case 0:
		return notThere(r)
	case 1:
	default:
		var names []string
		for _, e := range es {
			names = append(names, fmt.Sprintf("%q in %q (%s)", e.Name, e.Group, e.UUID))
		}
		return fmt.Errorf("%w: %d entries have the URL %s, none deleted: %s", ErrAmbiguous, len(es), r.Entry, strings.Join(names, "; "))
	}
	if !validUUID(es[0].UUID) {
		return errors.New("keepassxc: the entry has no usable uuid")
	}
	var res struct{}
	if err := s.k.call("delete-entry", map[string]any{"uuid": es[0].UUID}, &res); err != nil {
		if errors.Is(err, errNotSucceeded) {
			return fmt.Errorf("keepassxc: KeePassXC did not delete the entry (you declined its dialog, or the entry is gone): %w", err)
		}
		return waiting(err)
	}
	return nil
}
