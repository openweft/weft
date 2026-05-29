//go:build darwin && cgo

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	imock "github.com/openweft/hclconfig"
)

// ── rowToProto ────────────────────────────────────────────────────────────────

func TestRowToProto_MemConversion(t *testing.T) {
	// r.Mem is in GiB; proto MemMb must be r.Mem * 1024.
	r := imock.Row{
		Name:  "test-vm",
		CPU:   4,
		Mem:   8, // 8 GiB
		Disk:  20,
		State: "running",
	}
	info := rowToProto(r)
	wantMemMb := uint64(8 * 1024)
	if info.MemMb != wantMemMb {
		t.Errorf("MemMb: got %d, want %d", info.MemMb, wantMemMb)
	}
}

func TestRowToProto_CpuPassthrough(t *testing.T) {
	r := imock.Row{CPU: 6, Mem: 4, Disk: 10}
	info := rowToProto(r)
	if info.Cpu != 6 {
		t.Errorf("Cpu: got %d, want 6", info.Cpu)
	}
}

func TestRowToProto_DiskPassthrough(t *testing.T) {
	r := imock.Row{CPU: 2, Mem: 2, Disk: 50}
	info := rowToProto(r)
	if info.DiskGb != 50 {
		t.Errorf("DiskGb: got %d, want 50", info.DiskGb)
	}
}

func TestRowToProto_ZeroMem(t *testing.T) {
	r := imock.Row{Mem: 0}
	info := rowToProto(r)
	if info.MemMb != 0 {
		t.Errorf("MemMb: got %d, want 0 for zero Mem", info.MemMb)
	}
}

// ── enrichVMConfig ────────────────────────────────────────────────────────────

func TestEnrichVMConfig_StoresAllFields(t *testing.T) {
	dir := t.TempDir()
	if err := enrichVMConfig(dir, map[string]interface{}{
		"cpu":     uint32(4),
		"mem_mb":  uint64(4096),
		"mem_gib": uint64(4),
		"disk_gb": uint64(20),
	}); err != nil {
		t.Fatalf("enrichVMConfig: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cases := map[string]float64{
		"cpu":     4,
		"mem_mb":  4096,
		"mem_gib": 4,
		"disk_gb": 20,
	}
	for k, want := range cases {
		got, ok := m[k].(float64)
		if !ok {
			t.Errorf("key %q missing or wrong type in config.json", k)
			continue
		}
		if got != want {
			t.Errorf("%s: got %v, want %v", k, got, want)
		}
	}
}

func TestEnrichVMConfig_PreservesExistingImage(t *testing.T) {
	dir := t.TempDir()
	// Write an existing config.json with an image field.
	initial := map[string]interface{}{"image": "https://example.com/img.raw", "data_disks": []interface{}{}}
	b, _ := json.Marshal(initial)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)

	if err := enrichVMConfig(dir, map[string]interface{}{
		"cpu":     uint32(2),
		"mem_gib": uint64(2),
	}); err != nil {
		t.Fatalf("enrichVMConfig: %v", err)
	}

	b2, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	var m map[string]interface{}
	_ = json.Unmarshal(b2, &m)

	if m["image"] != "https://example.com/img.raw" {
		t.Errorf("image field was overwritten: %v", m["image"])
	}
	if _, ok := m["cpu"]; !ok {
		t.Error("cpu field missing after enrich")
	}
}

func TestEnrichVMConfig_MemMbToMemGibRatio(t *testing.T) {
	// Ensure the caller-computed mem_gib = mem_mb / 1024 is consistent.
	dir := t.TempDir()
	const memMb = uint64(8192)
	memGiB := memMb / 1024
	if err := enrichVMConfig(dir, map[string]interface{}{
		"mem_mb":  memMb,
		"mem_gib": memGiB,
	}); err != nil {
		t.Fatalf("enrichVMConfig: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)

	mb, _ := m["mem_mb"].(float64)
	gib, _ := m["mem_gib"].(float64)
	if gib*1024 != mb {
		t.Errorf("mem_gib (%v) * 1024 != mem_mb (%v)", gib, mb)
	}
}
