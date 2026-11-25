package secretsource

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-macos/keychain"
)

// ErrExists means the entry is already there and Replace was not asked.
var ErrExists = errors.New("secretsource: the entry already exists (use --replace to overwrite it)")

// ErrNoStore means the reference cannot be written by this package.
var ErrNoStore = errors.New("secretsource: cannot store into this reference")

// StoreOptions are the knobs of [Store].
type StoreOptions struct {
	// Replace overwrites an existing entry; without it an existing one is ErrExists.
	Replace bool
	// Login is the user name of a KeePassXC entry (default: the URL's host).
	Login string
	// Group is the KeePassXC group a new entry goes into, as a path of names
	// below the root group ("Keys/FIDO"). Default: the group KeePassXC picks,
	// "KeePassXC-Browser Passwords" in 2.8.0 (BrowserService.cpp:57, created
	// under the root when absent).
	Group string
}

// Store writes secret under ref and never reads or prints it elsewhere:
//
//	keychain:SERVICE/ACCOUNT        a Keychain generic-password item
//	secretservice:SERVICE/ACCOUNT   a Secret Service item
//	keepassxc:URL                   a KeePassXC entry whose URL field is URL
//
// An existing entry is ErrExists unless o.Replace. file: is refused: this
// package exists to move secrets off plaintext files. A KeePassXC entry is
// created by the browser protocol's set-login, so it always carries the URL
// that finds it again.
func Store(ctx context.Context, ref string, secret []byte, o StoreOptions) error {
	r, err := Parse(ref)
	if err != nil {
		return err
	}
	if len(secret) == 0 {
		return fmt.Errorf("%w: the secret is empty", ErrEmpty)
	}
	if len(secret) > MaxSize {
		return fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxSize)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case r.Scheme == "file":
		return fmt.Errorf("%w: file: is plaintext on disk, which this tool is meant to get rid of", ErrNoStore)
	case r.Scheme == "keepassxc":
		return storeKeePassXC(ctx, r, secret, o)
	}
	if o.Login != "" || o.Group != "" {
		return fmt.Errorf("%w: a login or group applies to keepassxc: only", ErrNoStore)
	}
	if r.Scheme == "secretservice" && r.Attrs != "" {
		return storeAttrs(ctx, r, secret, o)
	}
	return storeKV(ctx, r, secret, o)
}

// storeAttrs writes a secretservice:KEY=VALUE item. The item carries exactly
// those attributes; an existing item that has MORE attributes than the
// reference names is found (the lookup is by subset) but is not the one that
// is replaced, because the Secret Service replaces only an item with the same
// attributes: name them all when replacing.
func storeAttrs(ctx context.Context, r Ref, secret []byte, o StoreOptions) error {
	b, err := call(ctx, func() ([]byte, error) { return secretserviceGetAttrs(r.AttrMap()) })
	switch {
	case err == nil:
		clear(b)
		if !o.Replace {
			return fmt.Errorf("%w: %s", ErrExists, r)
		}
	case !isSSNotFound(err):
		return mapSecretService(r, err)
	}
	_, err = call(ctx, func() ([]byte, error) { return nil, ssSetAttrs("secretsource: "+r.Attrs, r.AttrMap(), secret) })
	if err != nil {
		return mapSecretService(r, err)
	}
	return nil
}

// storeKV writes a keychain or secretservice item, refusing an existing one.
func storeKV(ctx context.Context, r Ref, secret []byte, o StoreOptions) error {
	get, set, mapErr := secretserviceGet, ssSet, mapSecretService
	if r.Scheme == "keychain" {
		get, set = keychainGet, keychainSet
		mapErr = mapKeychain
	}
	b, err := call(ctx, func() ([]byte, error) { return get(r.Service, r.Account) })
	if err == nil {
		clear(b)
		if !o.Replace {
			return fmt.Errorf("%w: %s", ErrExists, r)
		}
	} else if !errors.Is(err, keychain.ErrNotFound) && !isSSNotFound(err) {
		return mapErr(r, err)
	}
	_, err = call(ctx, func() ([]byte, error) { return nil, set(r.Service, r.Account, secret) })
	if err != nil {
		return mapErr(r, err)
	}
	return nil
}

var validUUID = regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString

func storeKeePassXC(ctx context.Context, r Ref, secret []byte, o StoreOptions) error {
	login := o.Login
	if login == "" {
		u, _ := url.Parse(r.Entry)
		if login = u.Hostname(); login == "" {
			return fmt.Errorf("%w: %s has no host to use as the user name: give --login", ErrBadRef, r)
		}
	}
	s, err := openSession(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	// Is there an entry with this URL already? Found by URL, never by uuid or
	// path (those crash KeePassXC 2.8.0 on an entry without a URL).
	es, err := s.logins(r.Entry)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	req := map[string]any{"url": r.Entry, "submitUrl": r.Entry, "login": login, "password": string(secret)}
	switch len(es) {
	case 0:
		if o.Group != "" {
			id, err := s.groupUUID(o.Group)
			if err != nil {
				return err
			}
			req["group"], req["groupUuid"] = o.Group, id
		}
	case 1:
		if !o.Replace {
			return fmt.Errorf("%w: %s (%q in group %q)", ErrExists, r, es[0].Name, es[0].Group)
		}
		if o.Group != "" {
			return fmt.Errorf("%w: --group does not apply to an existing entry (KeePassXC updates it in place)", ErrNoStore)
		}
		if !validUUID(es[0].UUID) {
			return errors.New("keepassxc: the existing entry has no usable uuid")
		}
		req["uuid"] = es[0].UUID
	default:
		var names []string
		for _, e := range es {
			names = append(names, fmt.Sprintf("%q in %q (%s)", e.Name, e.Group, e.UUID))
		}
		return fmt.Errorf("%w: %d entries have the URL %s: %s", ErrAmbiguous, len(es), r.Entry, strings.Join(names, "; "))
	}
	var res struct {
		Error string `json:"error"`
	}
	if err := s.k.call("set-login", req, &res); err != nil {
		return waiting(err)
	}
	if res.Error != "success" {
		return errors.New("keepassxc: KeePassXC did not store it (an update is refused when the values are unchanged, the entry has no user name, or you declined its dialog)")
	}
	return nil
}

// groupNode is a group in the reply of get-database-groups.
type groupNode struct {
	Name     string      `json:"name"`
	UUID     string      `json:"uuid"`
	Children []groupNode `json:"children"`
}

// groupTree is that reply: {"groups":{"groups":[root]}} (BrowserService.cpp
// getDatabaseGroups wrapped once more by handleGetDatabaseGroups).
type groupTree struct {
	Groups struct {
		Groups []groupNode `json:"groups"`
	} `json:"groups"`
}

func (s *session) groupUUID(path string) (string, error) {
	var t groupTree
	if err := s.k.call("get-database-groups", map[string]any{}, &t); err != nil {
		return "", waiting(err)
	}
	return resolveGroup(t, path)
}

// resolveGroup finds the group at path ("A/B": names below the root) and
// returns its uuid. A name shared by two siblings is ambiguous, never guessed.
func resolveGroup(t groupTree, path string) (string, error) {
	if len(t.Groups.Groups) != 1 {
		return "", errors.New("keepassxc: no root group in the reply")
	}
	n := t.Groups.Groups[0]
	for _, name := range strings.Split(path, "/") {
		var found []groupNode
		for _, c := range n.Children {
			if c.Name == name {
				found = append(found, c)
			}
		}
		switch {
		case name == "" || len(found) == 0:
			return "", fmt.Errorf("%w: no group %q in KeePassXC (names below the root, separated by /)", ErrNotFound, path)
		case len(found) > 1:
			return "", fmt.Errorf("%w: several groups are named %q", ErrAmbiguous, name)
		}
		n = found[0]
	}
	if !validUUID(n.UUID) {
		return "", errors.New("keepassxc: the group has no usable uuid")
	}
	return n.UUID, nil
}

// alphabet for Generate: letters and digits, so a result works as a FIDO2 PIN
// (any characters), a password and a typed-in code alike.
const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// MaxGenerate bounds Generate.
const MaxGenerate = 256

// Generate draws n random characters (no look-alikes: no 0 O 1 l I) from
// crypto/rand, without modulo bias. The caller wipes the result.
func Generate(n int) ([]byte, error) {
	if n < 1 || n > MaxGenerate {
		return nil, fmt.Errorf("secretsource: want between 1 and %d characters", MaxGenerate)
	}
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, err := rand.Int(randReader, max)
		if err != nil {
			clear(out)
			return nil, err
		}
		out[i] = alphabet[v.Int64()]
	}
	return out, nil
}
