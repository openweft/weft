package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openweft/weft"
)

// TestBuildStorageFactory_DefaultIsFile confirms the empty-backend
// case yields nil (Adapter wraps nil with its own file-backed default).
func TestBuildStorageFactory_DefaultIsFile(t *testing.T) {
	sf, err := buildStorageFactory(fileConfigTargets{})
	if err != nil {
		t.Fatalf("buildStorageFactory: %v", err)
	}
	if sf == nil {
		t.Fatal("buildStorageFactory returned nil")
	}
	if sf.new != nil {
		t.Errorf("default backend should produce nil factory.new (Adapter applies file default); got non-nil")
	}
}

// TestBuildStorageFactory_FileExplicit covers the explicit "file"
// case — also produces nil, same as the default.
func TestBuildStorageFactory_FileExplicit(t *testing.T) {
	sf, err := buildStorageFactory(fileConfigTargets{storageBackend: "file"})
	if err != nil {
		t.Fatalf("buildStorageFactory: %v", err)
	}
	if sf == nil || sf.new != nil {
		t.Errorf("backend=file should produce non-nil *storageFactory with nil .new; got sf=%v", sf)
	}
}

// TestBuildStorageFactory_EtcdNeedsEndpoints checks the validation
// rule: backend=etcd without endpoints is an operator typo, not a
// silent fall-through.
func TestBuildStorageFactory_EtcdNeedsEndpoints(t *testing.T) {
	_, err := buildStorageFactory(fileConfigTargets{
		storageBackend: "etcd",
		// etcdEndpoints intentionally empty
	})
	if err == nil {
		t.Fatal("backend=etcd with no endpoints should error")
	}
	if !strings.Contains(err.Error(), "no endpoints") {
		t.Errorf("error %q should mention 'no endpoints'", err)
	}
}

// TestBuildStorageFactory_EtcdBuildsClosure covers the happy path
// for etcd: a factory closure is produced and invoking it yields
// an EtcdStorage whose Key() respects the configured prefix.
//
// Gated by VZD_ETCD_TEST=1: the etcd v3 client now opens a real
// connection at buildStorageFactory time. Without a live etcd
// endpoint that would either block on the dial timeout or fail
// outright, neither of which is a useful unit-test signal.
func TestBuildStorageFactory_EtcdBuildsClosure(t *testing.T) {
	if os.Getenv("VZD_ETCD_TEST") == "" {
		t.Skip("set VZD_ETCD_TEST=1 with a reachable etcd to opt in")
	}
	sf, err := buildStorageFactory(fileConfigTargets{
		storageBackend: "etcd",
		etcdEndpoints:  []string{"https://etcd-dc1:2379"},
		etcdKeyPrefix:  "/vzd/test/",
	})
	if err != nil {
		t.Fatalf("buildStorageFactory: %v", err)
	}
	if sf == nil || sf.new == nil {
		t.Fatal("etcd factory should be non-nil")
	}
	t.Cleanup(func() { _ = sf.close() })
	s := sf.new("projects")
	es, ok := s.(*weft.EtcdStorage)
	if !ok {
		t.Fatalf("factory returned %T, want *weft.EtcdStorage", s)
	}
	if es.Key() != "/vzd/test/projects" {
		t.Errorf("Key = %q, want /vzd/test/projects", es.Key())
	}
}

// TestBuildStorageFactory_UnknownBackend pins the error path for
// future regressions.
func TestBuildStorageFactory_UnknownBackend(t *testing.T) {
	_, err := buildStorageFactory(fileConfigTargets{storageBackend: "consul"})
	if err == nil || !strings.Contains(err.Error(), "unknown storage backend") {
		t.Errorf("err = %v, want 'unknown storage backend'", err)
	}
}

// TestDisplayStorageBackend covers the human-readable rendering used
// in the startup log line.
func TestDisplayStorageBackend(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "file"},
		{"file", "file"},
		{"etcd", "etcd"},
	}
	for _, c := range cases {
		if got := displayStorageBackend(c.in); got != c.want {
			t.Errorf("displayStorageBackend(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ----------------------------------------------------------------
// HCL config parsing: storage { backend = "..." etcd { ... } }
// ----------------------------------------------------------------

// TestFileConfig_StorageFileBackend covers the simplest valid
// vzd.hcl with backend = "file".
func TestFileConfig_StorageFileBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vzd.hcl")
	if err := os.WriteFile(path, []byte(`
		storage {
		  backend = "file"
		}
	`), 0o600); err != nil {
		t.Fatal(err)
	}
	fc, _, err := loadFileConfig(path)
	if err != nil {
		t.Fatalf("loadFileConfig: %v", err)
	}
	if fc.Storage == nil {
		t.Fatal("Storage block missing from decoded fc")
	}
	if fc.Storage.Backend != "file" {
		t.Errorf("backend = %q, want file", fc.Storage.Backend)
	}
	if fc.Storage.Etcd != nil {
		t.Errorf("etcd sub-block should be nil for file backend; got %+v", fc.Storage.Etcd)
	}
}

// TestFileConfig_StorageEtcdBackend covers the full prod-config
// shape with the etcd sub-block.
func TestFileConfig_StorageEtcdBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vzd.hcl")
	if err := os.WriteFile(path, []byte(`
		storage {
		  backend = "etcd"
		  etcd {
		    endpoints  = [
		      "https://etcd-dc1.example.com:2379",
		      "https://etcd-dc2.example.com:2379",
		      "https://etcd-dc3.example.com:2379",
		    ]
		    username   = "vzd"
		    password   = "redacted"
		    key_prefix = "/vzd/prod/"
		  }
		}
	`), 0o600); err != nil {
		t.Fatal(err)
	}
	fc, _, err := loadFileConfig(path)
	if err != nil {
		t.Fatalf("loadFileConfig: %v", err)
	}
	if fc.Storage == nil || fc.Storage.Etcd == nil {
		t.Fatalf("Storage / Etcd block missing: %+v", fc.Storage)
	}
	if fc.Storage.Backend != "etcd" {
		t.Errorf("backend = %q, want etcd", fc.Storage.Backend)
	}
	if got := len(fc.Storage.Etcd.Endpoints); got != 3 {
		t.Errorf("endpoints count = %d, want 3", got)
	}
	if fc.Storage.Etcd.KeyPrefix != "/vzd/prod/" {
		t.Errorf("key_prefix = %q, want /vzd/prod/", fc.Storage.Etcd.KeyPrefix)
	}
	if fc.Storage.Etcd.Username != "vzd" {
		t.Errorf("username = %q, want vzd", fc.Storage.Etcd.Username)
	}
}

// TestApplyFileConfigDefaults_StorageBlock checks the overlay: a
// non-nil Storage block populates targets with the right fields.
func TestApplyFileConfigDefaults_StorageBlock(t *testing.T) {
	backend := "etcd"
	fc := fileConfig{
		Storage: &storageBlock{
			Backend: backend,
			Etcd: &etcdBlock{
				Endpoints: []string{"https://etcd:2379"},
				KeyPrefix: "/vzd/dev/",
			},
		},
	}
	tgt := fileConfigTargets{}
	applyFileConfigDefaults(fc, &tgt)
	if tgt.storageBackend != "etcd" {
		t.Errorf("storageBackend = %q, want etcd", tgt.storageBackend)
	}
	if len(tgt.etcdEndpoints) != 1 || tgt.etcdEndpoints[0] != "https://etcd:2379" {
		t.Errorf("etcdEndpoints = %v, want [https://etcd:2379]", tgt.etcdEndpoints)
	}
	if tgt.etcdKeyPrefix != "/vzd/dev/" {
		t.Errorf("etcdKeyPrefix = %q, want /vzd/dev/", tgt.etcdKeyPrefix)
	}
}

// TestApplyFileConfigDefaults_NATSAuthorization checks the overlay
// for the `nats_authorization { path = ..., admin_pubkey = ... }`
// block: path is tilde-expanded, admin_pubkey passes through.
func TestApplyFileConfigDefaults_NATSAuthorization(t *testing.T) {
	fc := fileConfig{
		NATSAuthorization: &natsAuthorizationBlock{
			Path:        "/etc/vzd/nats-authorization.conf",
			AdminPubkey: "UABCD234567",
		},
	}
	tgt := fileConfigTargets{}
	applyFileConfigDefaults(fc, &tgt)
	if tgt.natsAuthzPath != "/etc/vzd/nats-authorization.conf" {
		t.Errorf("natsAuthzPath = %q, want /etc/vzd/nats-authorization.conf", tgt.natsAuthzPath)
	}
	if tgt.natsAuthzAdminPubkey != "UABCD234567" {
		t.Errorf("natsAuthzAdminPubkey = %q, want UABCD234567", tgt.natsAuthzAdminPubkey)
	}
}

// TestApplyFileConfigDefaults_NATSAuthorization_Disabled pins the
// opt-out path: no block in the file means natsAuthzPath stays
// empty (auto-render off; operator drives via `vzc admin nats-authz`).
func TestApplyFileConfigDefaults_NATSAuthorization_Disabled(t *testing.T) {
	fc := fileConfig{}
	tgt := fileConfigTargets{}
	applyFileConfigDefaults(fc, &tgt)
	if tgt.natsAuthzPath != "" {
		t.Errorf("natsAuthzPath should be empty when block is absent, got %q", tgt.natsAuthzPath)
	}
}
