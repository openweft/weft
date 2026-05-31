package weft

import (
	"context"
	"strings"
	"testing"
	"time"

	drivers "github.com/openweft/weft-drivers"
)

// fakeHyp is a non-functional drivers.HypervisorDriver — enough to
// satisfy the type constraint on HostHandle.Hypervisor.
type fakeHyp struct{ id string }

func (fakeHyp) Name() string { return "fake" }

// These methods are stubbed via embedding in the helper below ; only
// .id is used for assertion in the tests.

func newFakeHandle(id string) *HostHandle {
	return &HostHandle{Hypervisor: fakeHypAdapter{id: id}}
}

// fakeHypAdapter satisfies drivers.HypervisorDriver via no-op
// methods. The id field lets tests assert which entry came back.
type fakeHypAdapter struct{ id string }

func (fakeHypAdapter) HostInfo(c context.Context) (drivers.HostInfo, error) {
	return drivers.HostInfo{}, nil
}
func (fakeHypAdapter) CreateVM(context.Context, drivers.VMSpec) error             { return nil }
func (fakeHypAdapter) StartVM(context.Context, string) error                      { return nil }
func (fakeHypAdapter) StopVM(context.Context, string) error                       { return nil }
func (fakeHypAdapter) DeleteVM(context.Context, string) error                     { return nil }
func (fakeHypAdapter) AttachDisk(context.Context, string, drivers.DiskSpec) error { return nil }
func (fakeHypAdapter) DetachDisk(context.Context, string, string) error           { return nil }
func (fakeHypAdapter) AttachNIC(context.Context, string, drivers.NICHandle) error { return nil }
func (fakeHypAdapter) DetachNIC(context.Context, string, string) error            { return nil }

// adapterForDispatchTests builds a bare-bones Adapter with just
// enough wired to drive the dispatch tests : the host registry
// (so HostHandleOnArch can look up Drivers) + the dispatch maps.
func adapterForDispatchTests(t *testing.T) *Adapter {
	t.Helper()
	reg := &hostRegistry{
		storage: noopStorage{},
		byUUID:  map[string]Host{},
		nameIdx: map[string]string{},
	}
	return &Adapter{hostReg: reg}
}

type noopStorage struct{}

func (noopStorage) Load(c context.Context) ([]byte, error)        { return nil, nil }
func (noopStorage) Save(c context.Context, blob []byte) error     { return nil }

func TestRegisterHostHandleSet_Validation(t *testing.T) {
	a := adapterForDispatchTests(t)
	if err := a.RegisterHostHandleSet("", map[string]*HostHandle{"vz": newFakeHandle("x")}); err == nil {
		t.Error("empty uuid must error")
	}
	if err := a.RegisterHostHandleSet("h1", nil); err == nil {
		t.Error("empty set must error")
	}
	if err := a.RegisterHostHandleSet("h1", map[string]*HostHandle{"vz": nil}); err == nil {
		t.Error("nil handle in set must error")
	}
}

func TestRegisterHostHandleSet_MirrorsPrimary(t *testing.T) {
	a := adapterForDispatchTests(t)
	vz := newFakeHandle("vz")
	qemu := newFakeHandle("qemu")
	if err := a.RegisterHostHandleSet("h1", map[string]*HostHandle{"qemu": qemu, "vz": vz}); err != nil {
		t.Fatalf("RegisterHostHandleSet : %v", err)
	}
	// Single-entry table now carries vz (primary in stable order).
	got, err := a.HostHandleOn("h1")
	if err != nil {
		t.Fatalf("HostHandleOn : %v", err)
	}
	if hf, ok := got.Hypervisor.(fakeHypAdapter); !ok || hf.id != "vz" {
		t.Errorf("primary mirror = %+v ; want vz", got.Hypervisor)
	}
}

func TestHostHandleOnArch_PicksMatchingKind(t *testing.T) {
	a := adapterForDispatchTests(t)
	// Register the host in the registry so HostHandleOnArch can map
	// arch → kind via the Drivers capability list.
	a.hostReg.byUUID["mac-1"] = Host{
		UUID: "mac-1", Hostname: "mac-1", State: HostStateActive,
		LastSeenAt: time.Now(), CreatedAt: time.Now(),
		Drivers: []HostDriver{
			{Kind: "vz", Arches: []string{"arm64"}},
			{Kind: "qemu", Arches: []string{"amd64", "riscv64"}},
		},
	}
	if err := a.RegisterHostHandleSet("mac-1", map[string]*HostHandle{
		"vz":   newFakeHandle("vz"),
		"qemu": newFakeHandle("qemu"),
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		arch string
		want string
	}{
		{"arm64", "vz"},
		{"amd64", "qemu"},
		{"riscv64", "qemu"},
	}
	for _, c := range cases {
		t.Run(c.arch, func(t *testing.T) {
			got, err := a.HostHandleOnArch("mac-1", c.arch)
			if err != nil {
				t.Fatalf("HostHandleOnArch(%q) : %v", c.arch, err)
			}
			hf, _ := got.Hypervisor.(fakeHypAdapter)
			if hf.id != c.want {
				t.Errorf("HostHandleOnArch(%q) → %q ; want %q", c.arch, hf.id, c.want)
			}
		})
	}

	// Unsupported arch on a multi-plugin host is an error.
	if _, err := a.HostHandleOnArch("mac-1", "loongarch64"); err == nil {
		t.Error("unsupported arch on multi-plugin host should error")
	} else if !strings.Contains(err.Error(), "no driver covering arch") {
		t.Errorf("err = %q ; want substring about arch coverage", err.Error())
	}
}

func TestHostHandleOnArch_FallsBackToSingleDriver(t *testing.T) {
	a := adapterForDispatchTests(t)
	// Single-plugin path : register via RegisterHostHandle (no Set).
	if err := a.RegisterHostHandle("linux-1", newFakeHandle("only")); err != nil {
		t.Fatal(err)
	}
	got, err := a.HostHandleOnArch("linux-1", "amd64")
	if err != nil {
		t.Fatalf("HostHandleOnArch on single-driver host : %v", err)
	}
	hf, _ := got.Hypervisor.(fakeHypAdapter)
	if hf.id != "only" {
		t.Errorf("single-driver fallback got %q ; want only", hf.id)
	}
}

func TestHostHandleOnArch_EmptyArchUsesPrimary(t *testing.T) {
	a := adapterForDispatchTests(t)
	a.hostReg.byUUID["mac-1"] = Host{UUID: "mac-1", State: HostStateActive, Drivers: []HostDriver{
		{Kind: "vz", Arches: []string{"arm64"}}, {Kind: "qemu", Arches: []string{"amd64"}},
	}}
	_ = a.RegisterHostHandleSet("mac-1", map[string]*HostHandle{
		"vz": newFakeHandle("vz"), "qemu": newFakeHandle("qemu"),
	})
	got, err := a.HostHandleOnArch("mac-1", "")
	if err != nil {
		t.Fatalf("empty arch : %v", err)
	}
	hf, _ := got.Hypervisor.(fakeHypAdapter)
	if hf.id != "vz" {
		t.Errorf("empty arch on multi-plugin → %q ; want vz (primary)", hf.id)
	}
}

func TestUnregisterHostHandle_ClearsBothTables(t *testing.T) {
	a := adapterForDispatchTests(t)
	_ = a.RegisterHostHandleSet("h1", map[string]*HostHandle{"vz": newFakeHandle("vz")})
	a.UnregisterHostHandle("h1")
	if _, err := a.HostHandleOn("h1"); err == nil {
		t.Error("HostHandleOn after Unregister should error")
	}
	a.driverDispatchMu.RLock()
	_, hasSet := a.driverDispatchSet["h1"]
	a.driverDispatchMu.RUnlock()
	if hasSet {
		t.Error("driverDispatchSet entry should be cleared too")
	}
}
