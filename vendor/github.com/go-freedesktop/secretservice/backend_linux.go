//go:build linux

package secretservice

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

// D-Bus names for the Secret Service, per
// https://specifications.freedesktop.org/secret-service/.
const (
	ssBus          = "org.freedesktop.secrets"
	svcPath        = "/org/freedesktop/secrets"
	ifaceService   = "org.freedesktop.Secret.Service"
	ifaceColl      = "org.freedesktop.Secret.Collection"
	ifaceItem      = "org.freedesktop.Secret.Item"
	ifacePrompt    = "org.freedesktop.Secret.Prompt"
	ifaceSession   = "org.freedesktop.Secret.Session"
	defaultAlias   = "/org/freedesktop/secrets/aliases/default"
	propItemLabel  = "org.freedesktop.Secret.Item.Label"
	propItemAttrs  = "org.freedesktop.Secret.Item.Attributes"
	attrServiceKey = "service"
	attrAccountKey = "account"
)

// secret is the Secret Service "(oayays)" structure: the session it was
// transferred over, algorithm parameters (empty for the plain algorithm), the
// value bytes and a content type.
type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// dbusBackend is a live, owned session-bus connection with an open plain
// session. One is created per public call and closed on return.
type dbusBackend struct {
	conn    *dbus.Conn
	session dbus.ObjectPath
}

func init() {
	openBackend = func() (backend, error) { return dialDBus() }
}

// dialDBus connects a private session bus, opens a plain (unencrypted) Secret
// Service session and returns a ready backend. Any failure to reach the bus or
// the daemon is reported as [ErrUnavailable] so callers never mistake an absent
// keyring for a stored secret.
func dialDBus() (backend, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("%w: session bus: %v", ErrUnavailable, err)
	}
	svc := conn.Object(ssBus, svcPath)
	var output dbus.Variant
	var session dbus.ObjectPath
	// Plain algorithm: input and output are empty variants; the value bytes
	// travel unencrypted over the (already local, peer-credential-gated) bus.
	err = svc.Call(ifaceService+".OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&output, &session)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: OpenSession: %v", ErrUnavailable, err)
	}
	return &dbusBackend{conn: conn, session: session}, nil
}

func (b *dbusBackend) close() error {
	// Best-effort session teardown, then drop the connection we own.
	_ = b.conn.Object(ssBus, b.session).Call(ifaceSession+".Close", 0).Err
	return b.conn.Close()
}

// attributes are the lookup key for an item: a stable {service, account} map.
func attributes(service, account string) map[string]string {
	return map[string]string{attrServiceKey: service, attrAccountKey: account}
}

// collection resolves the default collection and unlocks it, returning its
// object path.
func (b *dbusBackend) collection() (dbus.ObjectPath, error) {
	svc := b.conn.Object(ssBus, svcPath)
	var path dbus.ObjectPath
	if err := svc.Call(ifaceService+".ReadAlias", 0, "default").Store(&path); err != nil || path == "/" {
		// No alias resolved (or the daemon lacks ReadAlias): fall back to the
		// well-known default-alias path.
		path = defaultAlias
	}
	if err := b.unlock([]dbus.ObjectPath{path}); err != nil {
		return "/", err
	}
	return path, nil
}

// unlock unlocks the given objects, driving any prompt to completion.
func (b *dbusBackend) unlock(objs []dbus.ObjectPath) error {
	svc := b.conn.Object(ssBus, svcPath)
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := svc.Call(ifaceService+".Unlock", 0, objs).Store(&unlocked, &prompt); err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	if prompt != "/" {
		return b.prompt(prompt)
	}
	return nil
}

// prompt runs a Secret Service prompt to completion, returning an error if the
// user dismisses it.
func (b *dbusBackend) prompt(path dbus.ObjectPath) error {
	sig := make(chan *dbus.Signal, 1)
	b.conn.Signal(sig)
	defer b.conn.RemoveSignal(sig)
	if err := b.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(path),
		dbus.WithMatchInterface(ifacePrompt),
		dbus.WithMatchMember("Completed"),
	); err != nil {
		return fmt.Errorf("prompt match: %w", err)
	}
	if err := b.conn.Object(ssBus, path).Call(ifacePrompt+".Prompt", 0, "").Err; err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	for s := range sig {
		if s.Path != path || s.Name != ifacePrompt+".Completed" || len(s.Body) < 1 {
			continue
		}
		if dismissed, ok := s.Body[0].(bool); ok && dismissed {
			return fmt.Errorf("secretservice: prompt dismissed")
		}
		return nil
	}
	return nil
}

func (b *dbusBackend) set(service, account string, value []byte) error {
	return b.setAttrs(service+"/"+account, attributes(service, account), value)
}

func (b *dbusBackend) setAttrs(label string, attrs map[string]string, value []byte) error {
	collPath, err := b.collection()
	if err != nil {
		return err
	}
	props := map[string]dbus.Variant{
		propItemLabel: dbus.MakeVariant(label),
		propItemAttrs: dbus.MakeVariant(attrs),
	}
	sec := secret{Session: b.session, Parameters: []byte{}, Value: value, ContentType: "application/octet-stream"}
	coll := b.conn.Object(ssBus, collPath)
	var item, prompt dbus.ObjectPath
	if err := coll.Call(ifaceColl+".CreateItem", 0, props, sec, true).Store(&item, &prompt); err != nil {
		return fmt.Errorf("CreateItem: %w", err)
	}
	if prompt != "/" {
		return b.prompt(prompt)
	}
	return nil
}

// search returns every item path (unlocked then locked) matching the
// attributes, unlocking any locked matches so their secrets can be read.
func (b *dbusBackend) search(service, account string) ([]dbus.ObjectPath, error) {
	return b.searchAttrs(attributes(service, account))
}

func (b *dbusBackend) searchAttrs(attrs map[string]string) ([]dbus.ObjectPath, error) {
	svc := b.conn.Object(ssBus, svcPath)
	var unlocked, locked []dbus.ObjectPath
	if err := svc.Call(ifaceService+".SearchItems", 0, attrs).Store(&unlocked, &locked); err != nil {
		return nil, fmt.Errorf("SearchItems: %w", err)
	}
	if len(locked) > 0 {
		if err := b.unlock(locked); err != nil {
			return nil, err
		}
	}
	return append(unlocked, locked...), nil
}

func (b *dbusBackend) get(service, account string) ([]byte, error) {
	items, err := b.search(service, account)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return b.secretOf(items[0])
}

func (b *dbusBackend) getAttrs(attrs map[string]string) ([]byte, error) {
	items, err := b.searchAttrs(attrs)
	if err != nil {
		return nil, err
	}
	switch len(items) {
	case 0:
		return nil, ErrNotFound
	case 1:
		return b.secretOf(items[0])
	}
	return nil, fmt.Errorf("%w: %d items", ErrAmbiguous, len(items))
}

func (b *dbusBackend) secretOf(item dbus.ObjectPath) ([]byte, error) {
	var sec secret
	if err := b.conn.Object(ssBus, item).Call(ifaceItem+".GetSecret", 0, b.session).Store(&sec); err != nil {
		return nil, fmt.Errorf("GetSecret: %w", err)
	}
	return sec.Value, nil
}

func (b *dbusBackend) delete(service, account string) error {
	items, err := b.search(service, account)
	if err != nil {
		return err
	}
	for _, it := range items {
		var prompt dbus.ObjectPath
		if err := b.conn.Object(ssBus, it).Call(ifaceItem+".Delete", 0).Store(&prompt); err != nil {
			return fmt.Errorf("Delete: %w", err)
		}
		if prompt != "/" {
			if err := b.prompt(prompt); err != nil {
				return err
			}
		}
	}
	return nil
}
