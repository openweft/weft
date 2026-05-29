//go:build darwin && cgo

package weft

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// makeTestAdapter creates a temporary stateDir and an Adapter pointing at it.
// Returns the adapter, the stateDir, and a pre-registered "test"
// project UUID that the rest of the helpers wire VMs into.
func makeTestAdapter(t *testing.T) (VZAdapter, string, string) {
	t.Helper()
	dir := t.TempDir()
	a := New(dir)
	p, _, err := a.(*Adapter).CreateProject("test")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return a, dir, p.UUID
}

// mkVMDir creates the VM subdirectory structure expected by ListLocal.
// VMs live at <base>/vz/<project-uuid>/<name>/ — the helper plants
// each test VM under the caller-supplied project UUID.
func mkVMDir(t *testing.T, base, projectUUID, name string) string {
	t.Helper()
	dir := filepath.Join(base, "vz", projectUUID, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkVMDir: %v", err)
	}
	return dir
}

// TestListLocal_EmptyVmsDir returns an empty map when the vms directory is absent.
func TestListLocal_EmptyVmsDir(t *testing.T) {
	a, _, _ := makeTestAdapter(t)
	m, err := a.ListLocal()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("want empty map, got %v", m)
	}
}

// TestListLocal_NoCpuMemDisk_Defaults verifies that when config.json has no
// cpu/mem/disk fields, ListLocal fills in the runvm.go defaults (2 vCPU,
// 2048 MB RAM) and leaves disk_gb absent when no disk.img exists.
func TestListLocal_NoCpuMemDisk_Defaults(t *testing.T) {
	a, base, projUUID := makeTestAdapter(t)
	dir := mkVMDir(t, base, projUUID, "vm1")

	cfg := map[string]interface{}{"image": "https://example.com/image.img"}
	writeJSON(t, filepath.Join(dir, "config.json"), cfg)

	m, err := a.ListLocal()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	props := m[projUUID+"/vm1"]
	if props == nil {
		t.Fatal("vm1 not in result")
	}

	if got, _ := props["cpu"].(float64); got != 2 {
		t.Errorf("cpu: got %v, want 2", got)
	}
	if got, _ := props["mem_mb"].(float64); got != 2048 {
		t.Errorf("mem_mb: got %v, want 2048", got)
	}
}

// TestListLocal_MemGibConversion verifies that a mem_gib stored in config.json
// is correctly converted to mem_mb (×1024) when mem_mb is absent.
func TestListLocal_MemGibConversion(t *testing.T) {
	a, base, projUUID := makeTestAdapter(t)
	dir := mkVMDir(t, base, projUUID, "vm2")

	writeJSON(t, filepath.Join(dir, "config.json"), map[string]interface{}{
		"cpu":     float64(4),
		"mem_gib": float64(8),
	})

	m, err := a.ListLocal()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	props := m[projUUID+"/vm2"]
	if got, _ := props["mem_mb"].(float64); got != 8*1024 {
		t.Errorf("mem_mb: got %v, want %v", got, 8*1024)
	}
}

// TestListLocal_StoredValues verifies that when config.json already contains
// cpu/mem_mb/disk_gb, ListLocal uses them as-is (no override).
func TestListLocal_StoredValues(t *testing.T) {
	a, base, projUUID := makeTestAdapter(t)
	dir := mkVMDir(t, base, projUUID, "vm3")

	writeJSON(t, filepath.Join(dir, "config.json"), map[string]interface{}{
		"cpu":     float64(6),
		"mem_mb":  float64(12288),
		"mem_gib": float64(12),
		"disk_gb": float64(50),
	})

	m, err := a.ListLocal()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	props := m[projUUID+"/vm3"]

	if got, _ := props["cpu"].(float64); got != 6 {
		t.Errorf("cpu: got %v, want 6", got)
	}
	if got, _ := props["mem_mb"].(float64); got != 12288 {
		t.Errorf("mem_mb: got %v, want 12288", got)
	}
	if got, _ := props["disk_gb"].(float64); got != 50 {
		t.Errorf("disk_gb: got %v, want 50", got)
	}
}

// TestListLocal_DiskSizeFromFile verifies that when disk_gb is absent but
// disk.img is present, ListLocal derives disk_gb from the file size.
func TestListLocal_DiskSizeFromFile(t *testing.T) {
	a, base, projUUID := makeTestAdapter(t)
	dir := mkVMDir(t, base, projUUID, "vm4")

	// Write a 2 GiB synthetic disk file (sparse, via truncate).
	diskPath := filepath.Join(dir, "disk.img")
	twoGiB := int64(2 * 1024 * 1024 * 1024)
	f, err := os.Create(diskPath)
	if err != nil {
		t.Fatalf("create disk.img: %v", err)
	}
	if err := f.Truncate(twoGiB); err != nil {
		f.Close()
		t.Fatalf("truncate disk.img: %v", err)
	}
	f.Close()

	// No config.json — disk_gb must come from the file stat.
	m, err := a.ListLocal()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	props := m[projUUID+"/vm4"]
	got, _ := props["disk_gb"].(float64)
	want := float64(twoGiB) / (1024 * 1024 * 1024) // = 2.0
	if got != want {
		t.Errorf("disk_gb: got %v, want %v", got, want)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func writeJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("writeJSON marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("writeJSON write: %v", err)
	}
}
