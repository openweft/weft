//go:build darwin

package weft

// listlocal_edge_test.go covers the ListLocal branches the
// existing adapter_listlocal_test.go doesn't reach: a running VM
// (vm.pid → live process), a non-UUID subdir (skipped), the
// registry file (skipped), and a malformed config.json (ignored).

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestListLocal_RunningPidAndSkips(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("proj")
	base := a.vmsDir()

	// 1. A VM whose vm.pid points at THIS test process → "running".
	runningDir := filepath.Join(base, p.UUID, "running-vm")
	if err := os.MkdirAll(runningDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(runningDir, "vm.pid"),
		[]byte(strconv.Itoa(os.Getpid())), 0o600)

	// 2. A VM with a malformed config.json → still listed, defaults applied.
	badCfgDir := filepath.Join(base, p.UUID, "bad-cfg-vm")
	_ = os.MkdirAll(badCfgDir, 0o700)
	_ = os.WriteFile(filepath.Join(badCfgDir, "config.json"), []byte("{not-json"), 0o600)

	// 3. A non-UUID project dir → skipped entirely.
	_ = os.MkdirAll(filepath.Join(base, "not-a-uuid-dir", "vmx"), 0o700)

	// 4. A stray file at top level (the registry file shape) → skipped.
	_ = os.WriteFile(filepath.Join(base, ".projects.hcl"), []byte("x"), 0o600)

	m, err := a.ListLocal()
	if err != nil {
		t.Fatalf("ListLocal: %v", err)
	}

	runKey := p.UUID + "/running-vm"
	if props, ok := m[runKey]; !ok {
		t.Fatalf("running VM missing from result")
	} else if props["State"] != "running" {
		t.Errorf("State = %v, want running", props["State"])
	}

	if _, ok := m[p.UUID+"/bad-cfg-vm"]; !ok {
		t.Errorf("bad-cfg VM should still be listed")
	}

	// The non-UUID dir's VM must NOT appear under any key.
	for k := range m {
		if k == "not-a-uuid-dir/vmx" {
			t.Errorf("non-UUID project dir should be skipped")
		}
	}
}

// TestListLocal_DeadPid covers the path where vm.pid exists but the
// process is gone → state stays "stopped".
func TestListLocal_DeadPid(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("proj")
	dir := filepath.Join(a.vmsDir(), p.UUID, "dead-vm")
	_ = os.MkdirAll(dir, 0o700)
	// PID 2^31-1 is essentially guaranteed not to exist.
	_ = os.WriteFile(filepath.Join(dir, "vm.pid"), []byte("2147483646"), 0o600)

	m, err := a.ListLocal()
	if err != nil {
		t.Fatal(err)
	}
	props := m[p.UUID+"/dead-vm"]
	if props == nil {
		t.Fatal("dead-vm missing")
	}
	if props["State"] != "stopped" {
		t.Errorf("State = %v, want stopped", props["State"])
	}
}
