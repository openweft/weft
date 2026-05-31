package agent

import (
	"io"
	"strings"
	"sync"
	"testing"

	weftplugin "github.com/openweft/weft-driver-plugin"
)

// TestBuildLocalHandlesMulti_HappyPath verifies that with two
// DriverSpec entries the helper launches one plugin per kind, keys
// the resulting map correctly, and reports "apple-vz" as the primary
// (vz wins over qemu in the stable ordering).
//
// The plugin_seam_test.go TestMain installs a fake launcher ; we
// extend it here with a counter so we can assert it ran twice.
func TestBuildLocalHandlesMulti_HappyPath(t *testing.T) {
	var (
		mu       sync.Mutex
		execs    []string
		original = launchDriverPlugin
	)
	launchDriverPlugin = func(opts weftplugin.LaunchOptions) (*weftplugin.DriverSet, io.Closer, error) {
		mu.Lock()
		execs = append(execs, opts.Executable)
		mu.Unlock()
		return &weftplugin.DriverSet{
			Hypervisor: seamHypervisor{}, Network: seamNetwork{},
			Volume: seamVolume{}, Image: seamImage{},
		}, seamCloser{}, nil
	}
	defer func() { launchDriverPlugin = original }()

	opts := Options{
		StateDir: t.TempDir(),
		Drivers: []DriverSpec{
			{Kind: "qemu", Arches: []string{"amd64", "riscv64"}}, // listed first but vz still wins primary
			{Kind: "vz", Arches: []string{"arm64"}},
		},
	}
	set, primary, closer, err := buildLocalHandlesMulti(opts, "host-uuid", "host-name")
	if err != nil {
		t.Fatalf("buildLocalHandlesMulti : %v", err)
	}
	defer closer.Close()

	if len(set) != 2 || set["vz"].Hypervisor == nil || set["qemu"].Hypervisor == nil {
		t.Errorf("set = %v ; want both vz + qemu keys with non-nil Hypervisor", set)
	}
	// Primary is the first by stable order — vz > qemu.
	if primary != "apple-vz" {
		t.Errorf("primary = %q ; want apple-vz (vz comes first in deterministic order)", primary)
	}
	// Executables : weft-driver-vz then weft-driver-qemu (stable order
	// wins over input order).
	mu.Lock()
	got := append([]string(nil), execs...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "weft-driver-vz" || got[1] != "weft-driver-qemu" {
		t.Errorf("launched executables = %v ; want [weft-driver-vz, weft-driver-qemu]", got)
	}
}

func TestBuildLocalHandlesMulti_NoDriversRejected(t *testing.T) {
	_, _, _, err := buildLocalHandlesMulti(Options{StateDir: t.TempDir()}, "u", "h")
	if err == nil {
		t.Fatal("empty Options.Drivers must error")
	}
}

func TestBuildLocalHandlesMulti_UnknownKindRejected(t *testing.T) {
	original := launchDriverPlugin
	launchDriverPlugin = func(weftplugin.LaunchOptions) (*weftplugin.DriverSet, io.Closer, error) {
		t.Error("launchDriverPlugin should not be called when kind is unknown")
		return nil, nil, nil
	}
	defer func() { launchDriverPlugin = original }()

	// "kvm" isn't in the {vz, qemu} vocabulary. The stable-order loop
	// over the known kinds simply skips it, and the post-loop check
	// reports no recognised kinds.
	_, _, _, err := buildLocalHandlesMulti(Options{
		StateDir: t.TempDir(),
		Drivers:  []DriverSpec{{Kind: "kvm"}},
	}, "u", "h")
	if err == nil {
		t.Fatal("unknown driver kind must error")
	}
	if !strings.Contains(err.Error(), "no recognised") {
		t.Errorf("err = %q ; want substring \"no recognised\"", err.Error())
	}
}

func TestBuildLocalHandlesMulti_LaunchErrorCleansUpStartedPlugins(t *testing.T) {
	// First launch succeeds (vz), second fails (qemu) — verify the
	// helper closes the first plugin so we don't leak subprocesses on
	// partial startup.
	var (
		closedCount int
		mu          sync.Mutex
		original    = launchDriverPlugin
	)
	closeRecorder := func() {
		mu.Lock()
		closedCount++
		mu.Unlock()
	}
	launchDriverPlugin = func(opts weftplugin.LaunchOptions) (*weftplugin.DriverSet, io.Closer, error) {
		switch opts.Executable {
		case "weft-driver-vz":
			return &weftplugin.DriverSet{
				Hypervisor: seamHypervisor{}, Network: seamNetwork{},
				Volume: seamVolume{}, Image: seamImage{},
			}, closerFunc(closeRecorder), nil
		case "weft-driver-qemu":
			return nil, nil, &launchErr{}
		}
		t.Fatalf("unexpected executable : %s", opts.Executable)
		return nil, nil, nil
	}
	defer func() { launchDriverPlugin = original }()

	_, _, _, err := buildLocalHandlesMulti(Options{
		StateDir: t.TempDir(),
		Drivers: []DriverSpec{
			{Kind: "vz"}, {Kind: "qemu"},
		},
	}, "u", "h")
	if err == nil {
		t.Fatal("expected error when second launch fails")
	}
	mu.Lock()
	defer mu.Unlock()
	if closedCount != 1 {
		t.Errorf("closedCount = %d ; want 1 (the first vz plugin must be cleaned up)", closedCount)
	}
}

type launchErr struct{}

func (launchErr) Error() string { return "synthetic launch failure" }
