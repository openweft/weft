package weft

import (
	"context"
	"errors"
	"strings"
	"testing"

	drivers "github.com/openweft/weft-drivers"
)

// fakeBlockVolume is a "block" VolumeDriver whose ListBackups returns a
// canned answer; every other method panics on the nil embedded interface.
type fakeBlockVolume struct {
	drivers.VolumeDriver
	backups []drivers.Backup
	err     error
}

func (fakeBlockVolume) Name() string { return blockDriverName }

func (f fakeBlockVolume) ListBackups(context.Context, string, string) ([]drivers.Backup, error) {
	return f.backups, f.err
}

func blockAdapter(t *testing.T, vd fakeBlockVolume) *Adapter {
	t.Helper()
	a := &Adapter{}
	if err := a.RegisterHostHandle("h1", &HostHandle{Volume: vd}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestVolumeBackupProject(t *testing.T) {
	ctx := context.Background()
	other := drivers.Backup{URL: "s3://x/other", Labels: map[string]string{"weft.project_uuid": "p-other"}}
	mine := drivers.Backup{URL: "s3://x/mine", Labels: map[string]string{"weft.project_uuid": "p-1"}}
	unlabelled := drivers.Backup{URL: "s3://x/bare"}

	a := blockAdapter(t, fakeBlockVolume{backups: []drivers.Backup{other, mine, unlabelled}})
	if p, found, err := a.VolumeBackupProject(ctx, "s3://x/mine"); err != nil || !found || p != "p-1" {
		t.Errorf("owned backup = (%q, %v, %v), want (p-1, true, nil)", p, found, err)
	}
	if p, found, err := a.VolumeBackupProject(ctx, "s3://x/bare"); err != nil || found || p != "" {
		t.Errorf("unlabelled backup = (%q, %v, %v), want (\"\", false, nil)", p, found, err)
	}
	if p, found, err := a.VolumeBackupProject(ctx, "s3://x/missing"); err != nil || found || p != "" {
		t.Errorf("absent backup = (%q, %v, %v), want (\"\", false, nil)", p, found, err)
	}
	if _, _, err := a.VolumeBackupProject(ctx, ""); err == nil {
		t.Error("empty url: want an error")
	}

	boom := errors.New("boom")
	failing := blockAdapter(t, fakeBlockVolume{err: boom})
	if _, _, err := failing.VolumeBackupProject(ctx, "s3://x/mine"); !errors.Is(err, boom) {
		t.Errorf("driver error not propagated: %v", err)
	}
	if _, _, err := (&Adapter{}).VolumeBackupProject(ctx, "s3://x/mine"); err == nil {
		t.Error("no block driver: want an error")
	}
}

// failingSaveStorage loads a seed and refuses every Save.
type failingSaveStorage struct{ *MemStorage }

func (failingSaveStorage) Save(context.Context, []byte) error { return errors.New("disk full") }

// A purge that cannot be written must fail the load, never keep serving a
// registry whose stored bytes still hold the secret.
func TestBucketRegistry_PurgeFailureFailsLoad(t *testing.T) {
	old := `{"buckets":[{"uuid":"u","project_uuid":"p","name":"n","endpoint":"e","secret_access_key":"s"}]}`
	_, err := loadBucketRegistry(context.Background(), failingSaveStorage{NewMemStorageWith([]byte(old))})
	if err == nil || !strings.Contains(err.Error(), "purge") {
		t.Fatalf("load err = %v, want a purge failure", err)
	}
}
