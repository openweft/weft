//go:build darwin

package weft

// final_fill_test.go: last batch of reachable branch coverage —
// nil-registry guards, clonefile share, migration skips, host-uuid
// empty-file path, autoRender error path, ListCachedImages error.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// SetNetworkDefaultSecurityGroups: network exists but sgReg is nil
// → second guard fires. We craft an Adapter with a network reg but
// no sg reg.
func TestSetNetworkDefaultSecurityGroups_NilSGReg(t *testing.T) {
	netReg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	a := &Adapter{networkReg: netReg, bus: NewEventBus()}
	// sgReg is nil → guard fires before any lookup.
	if err := a.SetNetworkDefaultSecurityGroups("any", []string{"x"}); err == nil {
		t.Errorf("nil sgReg should error")
	}
}

// SetNetworkDefaultSecurityGroups: unknown network UUID.
func TestSetNetworkDefaultSecurityGroups_UnknownNetwork(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.SetNetworkDefaultSecurityGroups("does-not-exist", nil); err == nil {
		t.Errorf("unknown network should error")
	}
}

// RegisterMicroVM with a Clone-flagged share exercises the
// unix.Clonefile path (APFS clone of the source tree into vmDir).
// macOS /tmp (t.TempDir) is APFS so the clone succeeds.
func TestAdapter_RegisterMicroVM_CloneShare(t *testing.T) {
	a := newAdapterForRegistries(t)
	_ = installFakeLocalHypervisor(t, a)

	src := t.TempDir()
	iso := filepath.Join(src, "boot.iso")
	_ = os.WriteFile(iso, []byte("iso-bytes"), 0o600)
	// A source tree to clone into the VM dir.
	shareSrc := filepath.Join(t.TempDir(), "rootfs")
	_ = os.MkdirAll(shareSrc, 0o700)
	_ = os.WriteFile(filepath.Join(shareSrc, "file"), []byte("payload"), 0o600)

	p, _, _ := a.CreateProject("p")
	err := a.RegisterMicroVM(p.UUID, "clone-vm", MicroVMBoot{BootISO: iso}, []MicroVMShare{
		{Tag: "rootfs0", Path: shareSrc, Clone: true},
	})
	if err != nil {
		t.Fatalf("RegisterMicroVM with clone share: %v", err)
	}
	// The clone lands at <vmDir>/<Tag>.
	cloned := filepath.Join(a.vmDirIn(p.UUID, "clone-vm"), "rootfs0")
	if _, err := os.Stat(filepath.Join(cloned, "file")); err != nil {
		t.Errorf("clone tree missing: %v", err)
	}
}

// migrateLegacyLayout skips non-directory entries + dirs lacking
// the flat-layout signature files.
func TestMigrateLegacyLayout_SkipsNonMatches(t *testing.T) {
	tmp := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(tmp, factory).(*Adapter)
	base := a.vmsDir()
	// A stray file at top level (not a dir) → skipped.
	_ = os.WriteFile(filepath.Join(base, "stray-file"), []byte("x"), 0o600)
	// A dir without machine-id.bin → skipped.
	_ = os.MkdirAll(filepath.Join(base, "incomplete-vm"), 0o700)
	_ = os.WriteFile(filepath.Join(base, "incomplete-vm", "config.json"), []byte("{}"), 0o600)
	// Must not panic + must not migrate anything.
	a.migrateLegacyLayout()
	// incomplete-vm should still be where it was (not moved).
	if _, err := os.Stat(filepath.Join(base, "incomplete-vm")); err != nil {
		t.Errorf("incomplete dir should be untouched: %v", err)
	}
}

// migrateNamedProjectDirs skips entries that are already UUIDs +
// the registry file.
func TestMigrateNamedProjectDirs_SkipsUUIDAndRegistry(t *testing.T) {
	tmp := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(tmp, factory).(*Adapter)
	base := a.vmsDir()
	// A UUID-named dir → skipped (already migrated).
	uuidDir := filepath.Join(base, "f47ac10b-58cc-4372-a567-0e02b2c3d479")
	_ = os.MkdirAll(uuidDir, 0o700)
	// A stray file (not a dir) → skipped.
	_ = os.WriteFile(filepath.Join(base, "loose"), []byte("x"), 0o600)
	a.migrateNamedProjectDirs()
	// UUID dir should remain.
	if _, err := os.Stat(uuidDir); err != nil {
		t.Errorf("UUID dir should be untouched: %v", err)
	}
}

// HeartbeatHost on an unknown UUID surfaces the registry error.
func TestAdapter_HeartbeatHost_Unknown(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.HeartbeatHost("no-such-host"); err == nil {
		t.Errorf("heartbeat on unknown host should error")
	}
}

// loadOrCreateHostUUID: when stateDir can't be created (a file sits
// where the dir should be), MkdirAll fails → error.
func TestLoadOrCreateHostUUID_MkdirError(t *testing.T) {
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	// stateDir is <blocker>/sub — MkdirAll fails because blocker is a file.
	a := &Adapter{stateDir: filepath.Join(blocker, "sub")}
	if _, err := a.loadOrCreateHostUUID(); err == nil {
		t.Errorf("mkdir over a file should error")
	}
}

// EtcdStorage Close is a no-op when not owning the client.
func TestEtcdStorage_CloseNotOwned(t *testing.T) {
	// NewEtcdStorageWithClient sets owned=false → Close is a no-op
	// returning nil even with a nil client.
	s := NewEtcdStorageWithClient(nil, "/prefix/", "projects")
	if err := s.Close(); err != nil {
		t.Errorf("not-owned Close should be nil, got %v", err)
	}
	if s.Key() != "/prefix/projects" {
		t.Errorf("Key() = %q", s.Key())
	}
}

// NewEtcdStorage with no endpoints → error (the config-validation
// branch, no network needed).
func TestNewEtcdStorage_NoEndpoints(t *testing.T) {
	if _, err := NewEtcdStorage(context.Background(), EtcdConfig{}, "projects"); err == nil {
		t.Errorf("no endpoints should error")
	}
}

// migrateNamedProjectDirs: getOrCreate fails (failing project
// registry) for a named subdir → the `continue` branch fires and
// the dir is left where it was.
func TestMigrateNamedProjectDirs_GetOrCreateFails(t *testing.T) {
	tmp := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(tmp, factory).(*Adapter)
	// Swap in a failing project registry so getOrCreate errors.
	preg, _ := loadProjectRegistry(context.Background(), saveFailsStorage{})
	a.projects = preg
	base := a.vmsDir()
	src := filepath.Join(base, "named-team", "vm")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	a.migrateNamedProjectDirs()
	// Dir untouched because getOrCreate failed.
	if _, err := os.Stat(filepath.Join(base, "named-team")); err != nil {
		t.Errorf("named dir should remain after getOrCreate failure: %v", err)
	}
}

// loadOrCreateHostUUID: an existing-but-empty file forces a fresh
// mint (the empty-string branch).
func TestLoadOrCreateHostUUID_EmptyFileMintsFresh(t *testing.T) {
	tmp := t.TempDir()
	a := &Adapter{stateDir: tmp}
	// Pre-create an empty host-uuid file.
	_ = os.WriteFile(a.hostUUIDFile(), []byte("  \n"), 0o600)
	uuid, err := a.loadOrCreateHostUUID()
	if err != nil {
		t.Fatal(err)
	}
	if uuid == "" {
		t.Errorf("empty file should trigger a fresh mint")
	}
	if !isUUID(uuid) {
		t.Errorf("minted UUID malformed: %q", uuid)
	}
}

// autoRenderNATSAuthorization error path: a path whose parent can't
// be created (a file in the way) → MkdirAll fails.
func TestAutoRenderNATSAuthorization_MkdirError(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Put a regular file where a directory is needed.
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	// authzPath wants <blocker>/sub/nats.conf — MkdirAll fails
	// because `blocker` is a file.
	a.SetNATSAuthorizationFile(filepath.Join(blocker, "sub", "nats.conf"), "")
	if err := a.autoRenderNATSAuthorization(); err == nil {
		t.Errorf("mkdir over a file should error")
	}
}

// ListCachedImages error: cacheDir is a *file*, not a directory →
// ReadDir errors (and it's not IsNotExist) → ListCachedImages
// surfaces the error.
func TestListCachedImages_ReadDirError(t *testing.T) {
	a := newAdapterForRegistries(t)
	tmp := t.TempDir()
	cacheAsFile := filepath.Join(tmp, "cache-is-a-file")
	_ = os.WriteFile(cacheAsFile, []byte("x"), 0o600)
	a.SetPaths(cacheAsFile, a.vmsPath)
	if _, err := a.ListCachedImages(); err == nil {
		t.Errorf("ReadDir on a file should error")
	}
}

