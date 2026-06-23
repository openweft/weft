package driverplugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRef_Default(t *testing.T) {
	if got := Default().Ref("qemu"); got != "ghcr.io/openweft/weft-driver-qemu:latest" {
		t.Errorf("Ref(qemu) = %q", got)
	}
	if got := Default().Ref("vz"); got != "ghcr.io/openweft/weft-driver-vz:latest" {
		t.Errorf("Ref(vz) = %q", got)
	}
}

func TestRef_RegistryVersion(t *testing.T) {
	c := Config{Registry: "registry.example/team", Version: "v1.2.3"}
	if got := c.Ref("qemu"); got != "registry.example/team/weft-driver-qemu:v1.2.3" {
		t.Errorf("Ref = %q", got)
	}
}

func TestRef_PerHVOverrideWins(t *testing.T) {
	c := Config{Registry: "r", Version: "v", Refs: map[string]string{"vz": "ghcr.io/acme/custom-vz@sha256:abc"}}
	if got := c.Ref("vz"); got != "ghcr.io/acme/custom-vz@sha256:abc" {
		t.Errorf("override Ref(vz) = %q", got)
	}
	// A different hv still derives.
	if got := c.Ref("qemu"); got != "r/weft-driver-qemu:v" {
		t.Errorf("derived Ref(qemu) = %q", got)
	}
}

func TestApplyEnv_OverlaysAndPreserves(t *testing.T) {
	t.Setenv(EnvRegistry, "ghcr.io/myorg")
	t.Setenv(EnvVersion, "v9")
	t.Setenv("WEFT_DRIVER_QEMU_REF", "ghcr.io/myorg/qemu-special:edge")
	// EnvToken intentionally unset → existing value preserved.

	c := Config{Registry: "old", Version: "old", Token: "keepme", Refs: map[string]string{}}
	c.ApplyEnv()

	if c.Registry != "ghcr.io/myorg" {
		t.Errorf("Registry = %q", c.Registry)
	}
	if c.Version != "v9" {
		t.Errorf("Version = %q", c.Version)
	}
	if c.Token != "keepme" {
		t.Errorf("Token clobbered: %q", c.Token)
	}
	if got := c.Ref("qemu"); got != "ghcr.io/myorg/qemu-special:edge" {
		t.Errorf("env per-hv ref not applied: %q", got)
	}
	// vz had no override → derives from the new registry/version.
	if got := c.Ref("vz"); got != "ghcr.io/myorg/weft-driver-vz:v9" {
		t.Errorf("Ref(vz) = %q", got)
	}
}

// TestApplyEnv_WasmOverride proves the "wasm" hypervisor kind
// participates in the same env-override surface as vz/qemu —
// added when weft-driver-wasm V0.1 landed as the fallback backend
// for hosts without hardware virt.
func TestApplyEnv_WasmOverride(t *testing.T) {
	t.Setenv("WEFT_DRIVER_WASM_REF", "ghcr.io/myorg/wasm-edge:v1")
	c := Default()
	c.ApplyEnv()
	if got := c.Ref("wasm"); got != "ghcr.io/myorg/wasm-edge:v1" {
		t.Errorf("Ref(wasm) = %q ; want override", got)
	}
}

// TestRef_WasmDerived covers the no-override path : Ref("wasm")
// derives from Registry+Version just like vz/qemu.
func TestRef_WasmDerived(t *testing.T) {
	c := Config{Registry: "r", Version: "v"}
	if got := c.Ref("wasm"); got != "r/weft-driver-wasm:v" {
		t.Errorf("derived Ref(wasm) = %q", got)
	}
}

func TestFromEnv_DefaultsWhenUnset(t *testing.T) {
	t.Setenv(EnvRegistry, "")
	t.Setenv(EnvVersion, "")
	c := FromEnv()
	if c.Registry != DefaultRegistry || c.Version != DefaultVersion {
		t.Errorf("FromEnv defaults = %q %q", c.Registry, c.Version)
	}
}

// TestResolve_LocalFirst proves a binary present in $WEFT_PLUGIN_DIR is used
// verbatim and the OCI path is never consulted (no network in this test).
func TestResolve_LocalFirst(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "weft-driver-qemu")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPluginDir, dir)

	got, err := Resolve(context.Background(), "weft-driver-qemu", t.TempDir(), Default())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != bin {
		t.Errorf("Resolve = %q, want local %q", got, bin)
	}
}

func TestResolve_EmptyName(t *testing.T) {
	if _, err := Resolve(context.Background(), "", t.TempDir(), Default()); err == nil {
		t.Errorf("empty exec name should error")
	}
}

// TestResolve_NonExecutableLocalIgnored: a non-executable file of the right
// name in the plugin dir must NOT satisfy local resolution (it would fall
// through to the OCI pull). We assert it's not returned as the local hit.
func TestResolve_NonExecutableLocalIgnored(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "weft-driver-qemu")
	if err := os.WriteFile(bin, []byte("not exec"), 0o644); err != nil { // no +x
		t.Fatal(err)
	}
	t.Setenv(EnvPluginDir, dir)
	if p, ok := findLocal("weft-driver-qemu"); ok {
		t.Errorf("non-executable file should not be a local hit, got %q", p)
	}
}
