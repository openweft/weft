// Package secretsource resolves a secret (a FIDO2 PIN, a container password, a
// recovery code) from a REFERENCE instead of a plaintext file the caller has
// to leave on disk.
//
// A reference is "scheme:value":
//
//	file:PATH                    a regular file readable by its owner alone
//	keychain:SERVICE/ACCOUNT     a macOS Keychain generic-password item
//	secretservice:SERVICE/ACCOUNT  a freedesktop Secret Service item whose
//	                             attributes are service=SERVICE, account=ACCOUNT
//	                             (GNOME Keyring, KWallet, KeePassXC with "Secret
//	                             Service Integration" enabled)
//	systemd-creds:NAME           a systemd credential (Linux): the file NAME in
//	                             $CREDENTIALS_DIRECTORY, else NAME decrypted from
//	                             a credstore.encrypted directory by systemd-creds
//
// There is deliberately no scheme that runs a command of the caller's choosing:
// a reference reaches the program through a command-line flag, and a flag must
// never be able to start a process. (systemd-creds runs one fixed program,
// systemd-creds, with a validated name; the reference never picks the program
// or its other arguments.) A reference never holds the secret itself, so it may sit in argv,
// a shell history or a script; the secret travels only from the manager to the
// returned [Secret], which the caller wipes with [Secret.Close].
//
//	s, err := secretsource.Open(ctx, "keychain:fido/yubikey-5c")
//	if err != nil { ... }
//	defer s.Close()
//	use(s.Bytes())
//
// Every call is bounded: a secret longer than [MaxSize] is refused, and the
// call ends with the context (or after [Timeout] when the context has no
// deadline) even when the manager is waiting for the person to unlock it.
package secretsource

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/go-freedesktop/secretservice"
)

// MaxSize is the longest secret accepted, in bytes.
const MaxSize = 4096

// Timeout bounds a call whose context has no deadline. It is generous because
// a manager may be asking the person to unlock it.
const Timeout = 2 * time.Minute

// Sentinel errors, comparable with [errors.Is].
var (
	// ErrBadRef is a reference that does not parse.
	ErrBadRef = errors.New("secretsource: bad reference")
	// ErrScheme is a reference whose scheme is not supported.
	ErrScheme = errors.New("secretsource: unsupported scheme")
	// ErrNotFound means the manager answered and holds no such entry.
	ErrNotFound = errors.New("secretsource: no such entry")
	// ErrUnavailable means no manager could be reached: none running, the
	// wrong platform, or it is locked and refused to ask.
	ErrUnavailable = errors.New("secretsource: secret manager unavailable")
	// ErrEmpty means the entry exists and holds nothing.
	ErrEmpty = errors.New("secretsource: the entry is empty")
	// ErrTooLarge means the secret is longer than [MaxSize].
	ErrTooLarge = errors.New("secretsource: the secret is too long")
)

// Secret is a resolved secret. Its bytes live until [Secret.Close] overwrites
// them.
type Secret struct{ b []byte }

// Bytes returns the secret. The slice is valid until Close.
func (s Secret) Bytes() []byte { return s.b }

// Close overwrites the secret with zeros. It is safe to call twice and never
// fails; it returns an error only to satisfy io.Closer.
func (s Secret) Close() error {
	clear(s.b)
	return nil
}

// Ref is a parsed reference.
type Ref struct {
	// Scheme is "file", "keychain" or "secretservice".
	Scheme string
	// Path is the file path (file scheme).
	Path string
	// Service and Account name the entry (keychain, secretservice).
	Service, Account string
	// Entry names a KeePassXC entry (keepassxc): by-uuid/HEX, by-path/PATH
	// or a URL the entry holds.
	Entry string
	// Attrs is a secretservice lookup by arbitrary attributes, canonical
	// ("key=value,key=value", keys sorted); see [Ref.AttrMap].
	Attrs string
	// Name is a systemd credential name (systemd-creds).
	Name string
}

// String renders the reference back as written.
func (r Ref) String() string {
	switch {
	case r.Scheme == "file":
		return "file:" + r.Path
	case r.Scheme == "systemd-creds":
		return "systemd-creds:" + r.Name
	case r.Scheme == "keepassxc":
		return "keepassxc:" + r.Entry
	case r.Attrs != "":
		return r.Scheme + ":" + r.Attrs
	}
	return r.Scheme + ":" + r.Service + "/" + r.Account
}

// AttrMap returns the attributes of a secretservice:key=value,... reference.
func (r Ref) AttrMap() map[string]string {
	m := map[string]string{}
	for _, p := range strings.Split(r.Attrs, ",") {
		k, v, _ := strings.Cut(p, "=")
		m[k] = v
	}
	return m
}

// parseAttrs checks key=value[,key=value...]: keys and values non-empty and
// free of "," (there is no escaping), no key twice, at most 32 pairs. It
// returns the canonical form, sorted by key.
func parseAttrs(ref, v string) (string, error) {
	pairs := strings.Split(v, ",")
	if len(pairs) > secretservice.MaxAttributes {
		return "", fmt.Errorf("%w: %q: more than %d attributes", ErrBadRef, clip(ref), secretservice.MaxAttributes)
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		k, val, ok := strings.Cut(p, "=")
		if !ok || k == "" || val == "" || seen[k] {
			return "", fmt.Errorf("%w: %q: want secretservice:key=value[,key=value...] with each key once", ErrBadRef, clip(ref))
		}
		seen[k] = true
	}
	slices.Sort(pairs) // any fixed order will do; this one is idempotent
	return strings.Join(pairs, ","), nil
}

// parseEntry checks a keepassxc: value: the URL held by the entry, which
// KeePassXC matches against the entries' URL fields.
//
// KeePassXC also finds an entry by "keepassxc://by-uuid/HEX" and
// "keepassxc://by-path/Group/Title" (BrowserService::shouldIncludeEntry), but
// KeePassXC 2.8.0 CRASHES (SIGSEGV) when such a lookup finds an entry with no
// URL: sortPriority dereferences std::max_element of an empty list
// (BrowserService.cpp:1347, reached from findEntries and sortEntries). A PIN
// entry has no URL as often as not, so those forms are refused here; an entry
// found by its URL has one by construction.
func parseEntry(ref, v string) (string, error) {
	switch {
	case strings.HasPrefix(v, "by-uuid/"), strings.HasPrefix(v, "by-path/"), strings.HasPrefix(v, "keepassxc://"):
		return "", fmt.Errorf("%w: %q: lookups by uuid or path crash KeePassXC 2.8.0 on an entry without a URL; give the entry a URL (for example https://theca.invalid/vault) and use keepassxc:URL", ErrBadRef, clip(ref))
	case len(v) <= 1024 && strings.Contains(v, "://"):
		return v, nil
	}
	return "", fmt.Errorf("%w: %q: want keepassxc:URL, the URL field of the entry (scheme://...)", ErrBadRef, clip(ref))
}

// Parse reads a reference. It never touches a manager. A reference without a
// scheme is refused rather than guessed to be a path: "file:PATH" is how a
// path is written.
func Parse(ref string) (Ref, error) {
	scheme, value, ok := strings.Cut(ref, ":")
	if !ok {
		return Ref{}, fmt.Errorf("%w: %q has no scheme; write file:PATH, keychain:SERVICE/ACCOUNT, secretservice:SERVICE/ACCOUNT or keepassxc:URL", ErrBadRef, clip(ref))
	}
	if value == "" {
		return Ref{}, fmt.Errorf("%w: %q names nothing after the colon", ErrBadRef, clip(ref))
	}
	if hasControl(value) {
		return Ref{}, fmt.Errorf("%w: a control character in the reference", ErrBadRef)
	}
	switch scheme {
	case "file":
		return Ref{Scheme: scheme, Path: value}, nil
	case "keychain", "secretservice":
		if scheme == "secretservice" && strings.Contains(value, "=") {
			a, err := parseAttrs(ref, value)
			if err != nil {
				return Ref{}, err
			}
			return Ref{Scheme: scheme, Attrs: a}, nil
		}
		svc, acct, ok := strings.Cut(value, "/")
		if !ok || svc == "" || acct == "" {
			return Ref{}, fmt.Errorf("%w: %q: want %s:SERVICE/ACCOUNT", ErrBadRef, clip(ref), scheme)
		}
		return Ref{Scheme: scheme, Service: svc, Account: acct}, nil
	case "keepassxc":
		e, err := parseEntry(ref, value)
		if err != nil {
			return Ref{}, err
		}
		return Ref{Scheme: scheme, Entry: e}, nil
	case "systemd-creds":
		if err := checkCredName(value); err != nil {
			return Ref{}, fmt.Errorf("%w: %q: %v", ErrBadRef, clip(ref), err)
		}
		return Ref{Scheme: scheme, Name: value}, nil
	}
	return Ref{}, fmt.Errorf("%w: %q (supported: file, keychain, secretservice, keepassxc, systemd-creds)", ErrScheme, clip(scheme))
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// clip keeps a malformed reference short in an error message.
func clip(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// Open resolves ref. The returned Secret must be closed.
func Open(ctx context.Context, ref string) (Secret, error) {
	r, err := Parse(ref)
	if err != nil {
		return Secret{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return Secret{}, err
	}
	var b []byte
	switch r.Scheme {
	case "file":
		// The file backend validates itself (trailing newline, permissions).
		b, err = readFile(r.Path)
		if err != nil {
			return Secret{}, err
		}
		return Secret{b}, nil
	case "keepassxc":
		b, err = openKeePassXC(ctx, r)
	case "systemd-creds":
		b, err = openSystemdCreds(ctx, r.Name)
	case "keychain":
		b, err = call(ctx, func() ([]byte, error) { return keychainGet(r.Service, r.Account) })
		err = mapKeychain(r, err)
	default: // secretservice
		b, err = call(ctx, func() ([]byte, error) {
			if r.Attrs != "" {
				return secretserviceGetAttrs(r.AttrMap())
			}
			return secretserviceGet(r.Service, r.Account)
		})
		err = mapSecretService(r, err)
	}
	if err != nil {
		clear(b)
		return Secret{}, err
	}
	return check(r, b)
}

// check applies the bounds every manager answer is held to.
func check(r Ref, b []byte) (Secret, error) {
	if len(b) > MaxSize {
		clear(b)
		return Secret{}, fmt.Errorf("%w: %s holds more than %d bytes", ErrTooLarge, r, MaxSize)
	}
	if len(b) == 0 {
		return Secret{}, fmt.Errorf("%w: %s", ErrEmpty, r)
	}
	return Secret{b}, nil
}

// call runs a manager call that takes no context, and gives up when ctx ends.
// A call still waiting on the manager then finishes in the background; its
// answer is wiped, never used.
func call(ctx context.Context, fn func() ([]byte, error)) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := fn()
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-ctx.Done():
		go func() { clear((<-ch).b) }()
		return nil, fmt.Errorf("%w: gave up waiting for the secret manager (is it locked, waiting for you to unlock it?): %v", ErrUnavailable, ctx.Err())
	}
}
