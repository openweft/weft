//go:build darwin

package weft

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOrCreateHostUUID_PersistsAcrossInstances exercises
// the on-disk identity story: two Adapter constructions
// pointing at the same stateDir must resolve to the same UUID
// (otherwise the Host registry would gain a new entry on every
// restart).
func TestLoadOrCreateHostUUID_PersistsAcrossInstances(t *testing.T) {
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }

	// Note: each NewWithStorage call gets a *fresh* MemStorage so
	// the registry contents are NOT shared — but the host-uuid
	// file lives on disk under stateDir, which is. This proves
	// the UUID file is the source of identity, independent of
	// registry state.
	a1 := NewWithStorage(stateDir, factory).(*Adapter)
	uuid1, err := a1.loadOrCreateHostUUID()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if uuid1 == "" {
		t.Fatal("first load returned empty UUID")
	}

	a2 := NewWithStorage(stateDir, factory).(*Adapter)
	uuid2, err := a2.loadOrCreateHostUUID()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if uuid2 != uuid1 {
		t.Errorf("host UUID changed across restart: %q → %q", uuid1, uuid2)
	}

	// And the file itself contains the UUID (sanity check on
	// the persistence path).
	data, err := os.ReadFile(filepath.Join(stateDir, "host-uuid"))
	if err != nil {
		t.Fatalf("read host-uuid: %v", err)
	}
	if !strings.Contains(string(data), uuid1) {
		t.Errorf("host-uuid file content %q doesn't contain UUID %q", data, uuid1)
	}
}

// TestSelfRegisterHost_CreatesEntry confirms the local host
// shows up in the registry after NewWithStorage. Single-host
// invariant: there's always exactly one Host entry whose UUID
// matches the persisted host-uuid file.
func TestSelfRegisterHost_CreatesEntry(t *testing.T) {
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(stateDir, factory).(*Adapter)

	hosts := a.Hosts()
	if len(hosts) != 1 {
		t.Fatalf("expected exactly 1 host after self-register, got %d", len(hosts))
	}
	h := hosts[0]
	if h.Hypervisor != "apple-vz" {
		t.Errorf("self-registered hypervisor = %q, want apple-vz", h.Hypervisor)
	}
	if h.State != HostStateActive {
		t.Errorf("self-registered state = %q, want active", h.State)
	}
	// Capability list should contain at least nat + mesh — the
	// two we know we'll need for the current direction.
	wantTypes := map[string]bool{"nat": false, "mesh": false}
	for _, nt := range h.NetworkTypes {
		if _, ok := wantTypes[nt]; ok {
			wantTypes[nt] = true
		}
	}
	for k, found := range wantTypes {
		if !found {
			t.Errorf("self-registered host missing network type %q: %v", k, h.NetworkTypes)
		}
	}

	// The UUID matches the persisted host-uuid file.
	uuid, _ := a.loadOrCreateHostUUID()
	if h.UUID != uuid {
		t.Errorf("registry UUID %q != persisted UUID %q", h.UUID, uuid)
	}
}

// TestSelfRegisterHost_Idempotent verifies that constructing a
// second Adapter against the same stateDir (a "restart") does
// NOT create a duplicate Host entry — the persisted UUID drives
// idempotent re-registration through hostRegistry.register's
// UUID-match path.
//
// We need both Adapters to share the same hosts-registry blob
// for this test to be meaningful, so the factory closes over a
// single MemStorage per registry name.
func TestSelfRegisterHost_Idempotent(t *testing.T) {
	stateDir := t.TempDir()
	shared := make(map[string]Storage)
	factory := func(name string) Storage {
		if s, ok := shared[name]; ok {
			return s
		}
		s := NewMemStorage()
		shared[name] = s
		return s
	}

	a1 := NewWithStorage(stateDir, factory).(*Adapter)
	if got := len(a1.Hosts()); got != 1 {
		t.Fatalf("after first construction: %d hosts, want 1", got)
	}
	uuidBefore := a1.Hosts()[0].UUID
	createdBefore := a1.Hosts()[0].CreatedAt

	// "Restart" — same stateDir, same shared registry storage.
	a2 := NewWithStorage(stateDir, factory).(*Adapter)
	hosts := a2.Hosts()
	if len(hosts) != 1 {
		t.Fatalf("after restart: %d hosts, want 1", len(hosts))
	}
	if hosts[0].UUID != uuidBefore {
		t.Errorf("UUID changed on restart: %q → %q", uuidBefore, hosts[0].UUID)
	}
	if hosts[0].CreatedAt != createdBefore {
		t.Errorf("CreatedAt changed on restart: %v → %v", createdBefore, hosts[0].CreatedAt)
	}
}
