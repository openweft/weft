package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/go-authn/harden/privfile"
	"github.com/go-freedesktop/secretservice"
	"github.com/go-macos/keychain"
	"golang.org/x/crypto/nacl/box"
)

// ErrAmbiguous means the reference matches more than one KeePassXC entry; a
// secret is never picked among several.
var ErrAmbiguous = errors.New("secretsource: the reference matches several entries")

// The pieces, replaceable by tests.
var (
	keepassxcDial   = dialKeePassXC
	newAssocStore   = defaultAssocStore
	goos            = runtime.GOOS
	userTempDir     = os.TempDir
	getenv          = os.Getenv
	keychainSet     = func(s, a string, b []byte) error { return keychain.Set(s, a, b) }
	ssSet           = secretservice.Set
	ssSetAttrs      = secretservice.SetByAttributes
	ssAvailable     = secretservice.Available
	userConfigDir   = os.UserConfigDir
	currentUserName = func() string { return os.Getenv("USER") }
)

const sockName = "org.keepassxc.KeePassXC.BrowserServer"

// socketCandidates lists where KeePassXC listens, from BrowserShared.cpp
// localServerPath(): macOS the per-user temporary directory (not $TMPDIR),
// elsewhere on Unix $XDG_RUNTIME_DIR/app/org.keepassxc.KeePassXC/ with a
// symlink at the legacy $XDG_RUNTIME_DIR/ location.
func socketCandidates() ([]string, error) {
	switch goos {
	case "windows":
		return nil, fmt.Errorf("%w: keepassxc: on Windows (a named pipe) is not implemented yet", ErrUnavailable)
	case "darwin":
		return []string{userTempDir() + "/" + sockName}, nil
	}
	rt := getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = userTempDir() + "/runtime-" + currentUserName()
	}
	return []string{rt + "/app/org.keepassxc.KeePassXC/" + sockName, rt + "/" + sockName}, nil
}

func dialKeePassXC(ctx context.Context) (net.Conn, error) {
	paths, err := socketCandidates()
	if err != nil {
		return nil, err
	}
	var last error
	for _, p := range paths {
		if last = checkSocket(p); last != nil {
			continue
		}
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", p)
		if err == nil {
			if last = checkPeer(c); last == nil {
				return c, nil
			}
			c.Close()
			continue
		}
		last = err
	}
	return nil, fmt.Errorf("%w: KeePassXC is not running or Browser Integration is off (Settings > Browser Integration): %v", ErrUnavailable, last)
}

// validHash accepts what a database hash looks like (hex), because it names a
// stored association and comes from a peer.
var validHash = regexp.MustCompile(`^[0-9a-fA-F]{1,128}$`).MatchString

// entryURL is what get-logins is asked for: the URL as the entry holds it.
func (r Ref) entryURL() string { return r.Entry }

// session is a KeePassXC connection that has proved its association.
type session struct {
	k    *conn
	keys []map[string]string
}

func (s *session) close() { s.k.close() }

// openSession connects, finds which database is open, loads this machine's
// association with it and has KeePassXC confirm it (test-associate).
func openSession(ctx context.Context) (*session, error) {
	store := newAssocStore()
	nc, err := keepassxcDial(ctx)
	if err != nil {
		return nil, err
	}
	k, err := newConn(ctx, nc)
	if err != nil {
		return nil, waiting(err)
	}
	fail := func(err error) (*session, error) {
		k.close()
		return nil, err
	}
	hash, err := k.databaseHash()
	if err != nil {
		return fail(waiting(err))
	}
	a, err := store.load(hash)
	if err != nil {
		return fail(err)
	}
	if err := k.call("test-associate", map[string]any{"id": a.ID, "key": a.Key}, &struct{}{}); err != nil {
		if errors.Is(err, ErrNotAssociated) {
			return fail(fmt.Errorf("%w: KeePassXC no longer knows the association %q; run `secretsource keepassxc associate`", ErrNotAssociated, a.ID))
		}
		return fail(waiting(err))
	}
	return &session{k: k, keys: []map[string]string{{"id": a.ID, "key": a.Key}}}, nil
}

// loginEntry is an entry get-logins returned.
type loginEntry struct {
	Password string `json:"password"`
	Name     string `json:"name"`
	UUID     string `json:"uuid"`
	Group    string `json:"group"`
}

// logins asks KeePassXC for the entries whose URL field matches url. NEVER
// pass a keepassxc://by-uuid or by-path URL here (it crashes KeePassXC 2.8.0
// on an entry without a URL): Parse refuses them, and so does this.
func (s *session) logins(url string) ([]loginEntry, error) {
	if strings.HasPrefix(url, "keepassxc://") {
		return nil, fmt.Errorf("%w: %q", ErrBadRef, clip(url))
	}
	var res struct {
		Entries []loginEntry `json:"entries"`
	}
	if err := s.k.call("get-logins", map[string]any{"url": url, "keys": s.keys}, &res); err != nil {
		return nil, waiting(err)
	}
	return res.Entries, nil
}

func openKeePassXC(ctx context.Context, r Ref) ([]byte, error) {
	s, err := openSession(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	es, err := s.logins(r.entryURL())
	if err != nil {
		return nil, err
	}
	switch len(es) {
	case 0:
		return nil, fmt.Errorf("%w: no KeePassXC entry for %s", ErrNotFound, r)
	case 1:
		return []byte(es[0].Password), nil
	}
	return nil, fmt.Errorf("%w: %d entries for %s; name one: give a single entry that URL", ErrAmbiguous, len(es), r)
}

// waiting adds the usual reason a KeePassXC call does not come back.
func waiting(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return fmt.Errorf("%w (KeePassXC may be waiting for you: unlock the database or answer its dialog)", err)
	}
	return err
}

func (k *conn) databaseHash() (string, error) {
	var res struct {
		Hash string `json:"hash"`
	}
	if err := k.call("get-databasehash", map[string]any{}, &res); err != nil {
		return "", err
	}
	if !validHash(res.Hash) {
		return "", errors.New("keepassxc: the database hash is not hexadecimal")
	}
	return res.Hash, nil
}

// Association names what Associate stored.
type Association struct {
	// Database is KeePassXC's hash of the open database.
	Database string
	// ID is the name the person gave the connection in KeePassXC.
	ID string
}

// Associate registers this machine with the database open in KeePassXC. The
// person approves it in KeePassXC and names the connection; nothing else in
// this package ever creates trust. The association (the connection's name and
// an identification public key, no secret) is kept by the OS keychain on
// macOS, the Secret Service on Linux when one answers, otherwise a 0600 file
// in the user's configuration directory.
func Associate(ctx context.Context) (Association, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Timeout)
		defer cancel()
	}
	store := newAssocStore()
	nc, err := keepassxcDial(ctx)
	if err != nil {
		return Association{}, err
	}
	k, err := newConn(ctx, nc)
	if err != nil {
		return Association{}, waiting(err)
	}
	defer k.close()
	hash, err := k.databaseHash()
	if err != nil {
		return Association{}, waiting(err)
	}
	idPub, err := identityKey()
	if err != nil {
		return Association{}, err
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := k.call("associate", map[string]any{"key": b64(k.pub[:]), "idKey": b64(idPub[:])}, &res); err != nil {
		return Association{}, waiting(err)
	}
	if res.ID == "" || len(res.ID) > 256 {
		return Association{}, errors.New("keepassxc: the association has no usable name")
	}
	if err := store.save(hash, assoc{ID: res.ID, Key: b64(idPub[:])}); err != nil {
		return Association{}, err
	}
	return Association{Database: hash, ID: res.ID}, nil
}

// identityKey makes the identification public key. It is a value, not the
// pointer box.GenerateKey returns, so that nothing is dereferenced after a
// failure.
func identityKey() ([32]byte, error) {
	pub, _, err := box.GenerateKey(randReader)
	if err != nil {
		return [32]byte{}, err
	}
	return *pub, nil
}

// assoc is what is kept per database.
type assoc struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type assocStore interface {
	load(hash string) (assoc, error)
	save(hash string, a assoc) error
}

const assocService = "secretsource.keepassxc"

func defaultAssocStore() assocStore {
	switch {
	case goos == "darwin":
		return kvStore{get: keychainGet, set: keychainSet, where: "the macOS Keychain"}
	case goos == "linux" && ssAvailable():
		return kvStore{get: secretserviceGet, set: ssSet, where: "the Secret Service"}
	}
	return fileStore{}
}

// kvStore keeps an association in a (service, account) store.
type kvStore struct {
	get   func(service, account string) ([]byte, error)
	set   func(service, account string, secret []byte) error
	where string
}

func (s kvStore) load(hash string) (assoc, error) {
	b, err := s.get(assocService, hash)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) || errors.Is(err, secretservice.ErrNotFound) {
			return assoc{}, notAssociated(hash)
		}
		return assoc{}, fmt.Errorf("%w: reading the association from %s: %v", ErrUnavailable, s.where, err)
	}
	return parseAssoc(b)
}

func (s kvStore) save(hash string, a assoc) error {
	b, _ := json.Marshal(a)
	if err := s.set(assocService, hash, b); err != nil {
		return fmt.Errorf("keepassxc: keeping the association in %s: %w", s.where, err)
	}
	return nil
}

// fileStore keeps an association in <config dir>/secretsource/keepassxc-<hash>.json, mode 0600.
type fileStore struct{}

func (fileStore) path(hash string) (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "secretsource", "keepassxc-"+hash+".json"), nil
}

func (f fileStore) load(hash string) (assoc, error) {
	p, err := f.path(hash)
	if err != nil {
		return assoc{}, err
	}
	b, err := readFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return assoc{}, notAssociated(hash)
	}
	if err != nil {
		return assoc{}, err
	}
	return parseAssoc(b)
}

func (f fileStore) save(hash string, a assoc) error {
	p, err := f.path(hash)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkDir(dir); err != nil {
		return fmt.Errorf("keepassxc: not keeping the association: %w", err)
	}
	b, _ := json.Marshal(a)
	// privfile.Write checks the directory again at the write and writes a 0600
	// temporary file renamed onto p: a symbolic link left at p is replaced, not
	// followed, and a reader never sees half a file. On Windows the file gets an
	// owner-only DACL.
	if err := privfile.Write(p, b); err != nil {
		return fmt.Errorf("keepassxc: not keeping the association: %w", err)
	}
	return nil
}

func notAssociated(hash string) error {
	return fmt.Errorf("%w: run `secretsource keepassxc associate` once and approve it in KeePassXC (database %s)", ErrNotAssociated, hash)
}

func parseAssoc(b []byte) (assoc, error) {
	var a assoc
	if err := json.Unmarshal(b, &a); err != nil || a.ID == "" || a.Key == "" {
		return assoc{}, errors.New("keepassxc: the stored association is damaged; run `secretsource keepassxc associate`")
	}
	return a, nil
}
