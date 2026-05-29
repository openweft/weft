//go:build darwin

package weft

// rollback_test.go drives the Storage-error rollback branches in the
// registry create / setName / setState mutators. A saveFailsStorage
// loads empty (so the registry constructs cleanly) but fails every
// Save, forcing each mutator down its rollback path.

import (
	"context"
	"errors"
	"testing"
)

// saveFailsStorage: Load returns empty (fresh registry), Save always
// errors. Lets us exercise the "mutate in-memory, then save fails,
// then roll back" branch in every registry.
type saveFailsStorage struct{}

func (saveFailsStorage) Load(ctx context.Context) ([]byte, error) { return nil, nil }
func (saveFailsStorage) Save(ctx context.Context, blob []byte) error {
	return errors.New("save failure (test)")
}

func TestVolumeRegistry_CreateRollbackOnSaveError(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), saveFailsStorage{})
	if _, err := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "n", SizeGiB: 1}); err == nil {
		t.Fatal("expected save error")
	}
	// Rollback: registry should be empty again.
	if len(reg.byUUID) != 0 {
		t.Errorf("byUUID not rolled back: %v", reg.byUUID)
	}
	if len(reg.nameIdx) != 0 {
		t.Errorf("nameIdx not rolled back")
	}
	if len(reg.projectIdx) != 0 {
		t.Errorf("projectIdx not rolled back")
	}
}

func TestNetworkRegistry_CreateRollbackOnSaveError(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), saveFailsStorage{})
	if _, err := reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"}); err == nil {
		t.Fatal("expected save error")
	}
	if len(reg.byUUID) != 0 || len(reg.nameIdx) != 0 || len(reg.projectIdx) != 0 {
		t.Errorf("network registry not rolled back")
	}
}

func TestSecurityGroupRegistry_CreateRollbackOnSaveError(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), saveFailsStorage{})
	if _, err := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "n"}); err == nil {
		t.Fatal("expected save error")
	}
	if len(reg.byUUID) != 0 || len(reg.nameIdx) != 0 || len(reg.projectIdx) != 0 {
		t.Errorf("sg registry not rolled back")
	}
}

func TestVMRegistry_CreateRollbackOnSaveError(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), saveFailsStorage{})
	if _, err := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "n", HostUUID: "h"}); err == nil {
		t.Fatal("expected save error")
	}
	if len(reg.byUUID) != 0 || len(reg.nameIdx) != 0 || len(reg.projectIdx) != 0 {
		t.Errorf("vm registry not rolled back")
	}
}

func TestProjectRegistry_GetOrCreateRollbackOnSaveError(t *testing.T) {
	reg, _ := loadProjectRegistry(context.Background(), saveFailsStorage{})
	if _, _, err := reg.getOrCreate("p"); err == nil {
		t.Fatal("expected save error")
	}
	if len(reg.byUUID) != 0 || len(reg.nameIdx) != 0 {
		t.Errorf("project registry not rolled back")
	}
}

func TestUserRegistry_GetOrCreateRollbackOnSaveError(t *testing.T) {
	reg, _ := loadUserRegistry(context.Background(), saveFailsStorage{})
	if _, _, err := reg.getOrCreateFromCaller(&Caller{Subject: "s", Issuer: "i", Email: "x@x"}); err == nil {
		t.Fatal("expected save error")
	}
	if len(reg.byUUID) != 0 || len(reg.subjectIdx) != 0 {
		t.Errorf("user registry not rolled back")
	}
}

func TestProjectRegistry_EnsureNATSUserSeedRollbackOnSaveError(t *testing.T) {
	// First create a project with a working storage, then swap to a
	// failing one for the seed-mint save.
	mem := NewMemStorage()
	reg, _ := loadProjectRegistry(context.Background(), mem)
	p, _, _ := reg.getOrCreate("p")
	reg.storage = saveFailsStorage{}
	if _, err := reg.ensureNATSUserSeed(p.UUID); err == nil {
		t.Fatal("expected save error")
	}
	// Seed should be rolled back to empty.
	got, _ := reg.lookupByUUID(p.UUID)
	if got.NATSUserSeed != "" {
		t.Errorf("seed not rolled back: %q", got.NATSUserSeed)
	}
}

func TestHostRegistry_RegisterRollbackOnSaveError(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), saveFailsStorage{})
	if _, err := reg.register(RegisterHostSpec{Hostname: "h"}); err == nil {
		t.Fatal("expected save error")
	}
	if len(reg.byUUID) != 0 || len(reg.nameIdx) != 0 {
		t.Errorf("host registry not rolled back")
	}
}

func TestPortRegistry_CreateRollbackOnSaveError(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), saveFailsStorage{})
	_, err := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: "n",
		MAC: "02:00:00:00:00:01", IP: "10.0.0.5",
	})
	if err == nil {
		t.Fatal("expected save error")
	}
	if len(reg.byUUID) != 0 {
		t.Errorf("port registry not rolled back")
	}
}
