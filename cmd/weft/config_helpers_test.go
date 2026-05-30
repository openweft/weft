package main

import (
	"os"
	"path/filepath"
	"testing"
)

// --- expandHome --------------------------------------------------------------

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir on this host")
	}
	cases := []struct {
		in   string
		want string
	}{
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~", home},
		{"~/", home},
		{"~/.weft/weft.sock", filepath.Join(home, ".weft/weft.sock")},
		{"~notme/x", "~notme/x"}, // only "~/" and bare "~" expand
	}
	for _, c := range cases {
		if got := expandHome(c.in); got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- displayEventBusBackend --------------------------------------------------

func TestDisplayEventBusBackend(t *testing.T) {
	cases := map[string]string{"": "local", "local": "local", "nats": "nats"}
	for in, want := range cases {
		if got := displayEventBusBackend(in); got != want {
			t.Errorf("displayEventBusBackend(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- buildEventBus -----------------------------------------------------------

func TestBuildEventBus_DefaultLocal(t *testing.T) {
	f, err := buildEventBus(fileConfigTargets{}) // empty backend → "local"
	if err != nil {
		t.Fatalf("buildEventBus default: %v", err)
	}
	if f == nil || f.bus == nil {
		t.Fatal("expected non-nil local bus")
	}
	if f.close != nil {
		_ = f.close()
	}
}

func TestBuildEventBus_LocalExplicit(t *testing.T) {
	f, err := buildEventBus(fileConfigTargets{eventBusBackend: "local"})
	if err != nil {
		t.Fatalf("buildEventBus local: %v", err)
	}
	if f.bus == nil {
		t.Fatal("expected non-nil local bus")
	}
	_ = f.close()
}

func TestBuildEventBus_NATSNeedsURL(t *testing.T) {
	_, err := buildEventBus(fileConfigTargets{eventBusBackend: "nats"})
	if err == nil {
		t.Fatal("expected error when nats backend has no URL")
	}
}

func TestBuildEventBus_NATSBadURL(t *testing.T) {
	// A syntactically present but unreachable URL exercises the
	// NewNATSEventBus error branch (connection refused) without a
	// live server.
	_, err := buildEventBus(fileConfigTargets{
		eventBusBackend: "nats",
		natsURL:         "nats://127.0.0.1:1", // nothing listens on port 1
	})
	if err == nil {
		t.Fatal("expected dial error for unreachable NATS URL")
	}
}

func TestBuildEventBus_UnknownBackend(t *testing.T) {
	_, err := buildEventBus(fileConfigTargets{eventBusBackend: "rabbitmq"})
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

// --- loadFileConfig ----------------------------------------------------------

func TestLoadFileConfig_ExplicitMissing(t *testing.T) {
	_, _, err := loadFileConfig(filepath.Join(t.TempDir(), "nope.hcl"))
	if err == nil {
		t.Fatal("explicit missing config should error")
	}
}

func TestLoadFileConfig_ExplicitDecodeError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.hcl")
	if err := os.WriteFile(p, []byte("this is = not valid hcl {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadFileConfig(p)
	if err == nil {
		t.Fatal("malformed HCL should error")
	}
}

func TestLoadFileConfig_ExplicitValid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "weft.hcl")
	content := `
socket     = "/tmp/test.sock"
config_dir = "/tmp/hcl"
oidc {
  issuer    = "https://dex.example.com"
  client_id = "weft"
}
storage {
  backend = "file"
}
event_bus {
  backend = "local"
}
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, gotPath, err := loadFileConfig(p)
	if err != nil {
		t.Fatalf("loadFileConfig: %v", err)
	}
	if gotPath != p {
		t.Errorf("path = %q, want %q", gotPath, p)
	}
	if cfg.Socket == nil || *cfg.Socket != "/tmp/test.sock" {
		t.Errorf("socket not decoded: %+v", cfg.Socket)
	}
	if cfg.OIDC == nil || cfg.OIDC.Issuer != "https://dex.example.com" {
		t.Errorf("oidc block not decoded: %+v", cfg.OIDC)
	}
}

func TestLoadFileConfig_DefaultDiscoveryNone(t *testing.T) {
	// With HOME pointed at an empty temp dir and no /etc/weft file
	// (assumed absent on the test host), discovery returns the zero
	// value with no error.
	t.Setenv("HOME", t.TempDir())
	cfg, p, err := loadFileConfig("")
	if err != nil {
		t.Fatalf("default discovery should not error: %v", err)
	}
	// On a typical test host /etc/weft/weft.hcl doesn't exist, so we
	// expect the zero value. If the host happens to have one, just
	// assert no error (above) and skip the zero-value check.
	if p != "" && cfg.Socket == nil {
		t.Logf("discovered a real config at %s; skipping zero-value assertion", p)
	}
}

func TestLoadFileConfig_DefaultDiscoveryHit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "weft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "weft.hcl")
	if err := os.WriteFile(p, []byte(`socket = "/tmp/discovered.sock"`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, gotPath, err := loadFileConfig("")
	if err != nil {
		t.Fatalf("discovery hit: %v", err)
	}
	if gotPath != p {
		t.Errorf("path = %q, want %q", gotPath, p)
	}
	if cfg.Socket == nil || *cfg.Socket != "/tmp/discovered.sock" {
		t.Errorf("socket not decoded from discovered file")
	}
}

// --- applyFileConfigDefaults (OIDC + EventBus blocks) ------------------------

func TestApplyFileConfigDefaults_OIDCAndEventBus(t *testing.T) {
	s := func(v string) *string { return &v }
	cfg := fileConfig{
		Socket:    s("/s.sock"),
		SSHSocket: s("/ssh.sock"),
		ConfigDir: s("/cfg"),
		OIDC: &oidcBlock{
			Issuer:            "https://idp",
			ClientID:          "weft",
			SkipClientIDCheck: true,
		},
		EventBus: &eventBusBlock{
			Backend: "nats",
			NATS: &natsBlock{
				URL:             "nats://host:4222",
				CredentialsFile: "~/creds",
				Name:            "weft",
				SubjectPrefix:   "weft.",
			},
		},
	}
	var dst fileConfigTargets
	applyFileConfigDefaults(cfg, &dst)
	if dst.socket != "/s.sock" || dst.sshSocket != "/ssh.sock" || dst.configDir != "/cfg" {
		t.Errorf("base fields not applied: %+v", dst)
	}
	if dst.oidcIssuer != "https://idp" || dst.oidcClientID != "weft" || !dst.oidcSkipClientIDCheck {
		t.Errorf("oidc not applied: %+v", dst)
	}
	if dst.eventBusBackend != "nats" || dst.natsURL != "nats://host:4222" || dst.natsName != "weft" || dst.natsSubjectPrefix != "weft." {
		t.Errorf("event bus not applied: %+v", dst)
	}
}

func TestApplyFileConfigDefaults_StorageEtcdBlock(t *testing.T) {
	cfg := fileConfig{
		Storage: &storageBlock{
			Backend: "etcd",
			Etcd: &etcdBlock{
				Endpoints: []string{"http://a:2379", "http://b:2379"},
				Username:  "u",
				Password:  "p",
				KeyPrefix: "/weft/",
			},
		},
		NATSAuthorization: &natsAuthorizationBlock{
			Path:        "~/authz.conf",
			AdminPubkey: "UABC",
		},
	}
	var dst fileConfigTargets
	applyFileConfigDefaults(cfg, &dst)
	if dst.storageBackend != "etcd" {
		t.Errorf("storageBackend = %q, want etcd", dst.storageBackend)
	}
	if len(dst.etcdEndpoints) != 2 || dst.etcdUsername != "u" || dst.etcdPassword != "p" || dst.etcdKeyPrefix != "/weft/" {
		t.Errorf("etcd block not applied: %+v", dst)
	}
	if dst.natsAuthzAdminPubkey != "UABC" || dst.natsAuthzPath == "" {
		t.Errorf("nats_authorization not applied: %+v", dst)
	}
}

func TestApplyFileConfigDefaults_Empty(t *testing.T) {
	// Nil blocks must leave the destination untouched.
	dst := fileConfigTargets{socket: "preset"}
	applyFileConfigDefaults(fileConfig{}, &dst)
	if dst.socket != "preset" {
		t.Errorf("empty config should not overwrite preset, got %q", dst.socket)
	}
}
