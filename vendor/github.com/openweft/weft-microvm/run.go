// Package microvm holds the microVM runtime logic: prepare an
// OCI-image rootfs on the host, then drive weft's weft gRPC API to
// register and start a microVM around it.
//
// Flow (single-container, see Run → runMicroVM):
//
//  1. Verify the image is pulled (rootfs/ present); auto-pull on miss.
//  2. Apply per-run command overrides to <rootfs>/.weft-microvm/config.json.
//  3. Locate the boot artefacts (kernel+initrd, or a UKI ISO).
//  4. Dial weft's Unix gRPC socket (default ~/.weft/weft.sock).
//  5. RegisterMicroVM(name, boot artefacts, virtio-fs rootfs share).
//  6. StartVM(name).
//
// The VM ends up in weft's inventory and is controllable through any
// other client (e.g. `weft instance status …`). This package exposes
// a typed API — flag parsing lives in the CLI front-end (weft).

package microvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openweft/weft-client"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// Args holds the typed inputs for Run. The CLI front-end populates
// it from parsed flags/positionals; library callers build it
// directly.
type Args struct {
	// Image is the OCI reference to boot (e.g. "alpine:3.21" or
	// "ghcr.io/example/app:v1.2.3"). Required in single-container
	// mode (i.e. when Pod is empty).
	Image string

	// Cmd overrides the image's entrypoint+cmd when non-nil
	// (equivalent to the `-- CMD…` tail in a CLI front-end, e.g.
	// Cmd=["sh","-c","echo hi"]).
	Cmd []string

	// Detach, when true, returns once the VM is alive rather than
	// streaming stdio until exit. Not implemented yet.
	Detach bool

	// MountTag is the virtio-fs tag exposed inside the guest.
	// Default "rootfs0"; rarely changed but useful for parallel
	// runs sharing the same kernel.
	MountTag string

	// WeftSocket overrides the path to weft's Unix gRPC socket.
	// Empty means use the default ~/.weft/weft.sock (same as weft).
	WeftSocket string

	// Project is the multitenant namespace the new microVM lands
	// in. Empty resolves to weft's default project (currently
	// `usr-<weft-os-user>`); a string like `team-net` lands the VM
	// in a shared project. Phase-2 auth will gate access here.
	Project string

	// Pod, when non-empty, is the path to a pod manifest JSON
	// (multi-container mode). Mutually exclusive with Image and
	// Cmd: the manifest names its own images and commands. Pod mode
	// boots weft-init (a supervisor PID 1) instead of weft-microvm-init.
	Pod string
}

// Run is the typed entry point: dispatch to pod mode (multi-
// container, weft-init PID 1) when Args.Pod is set, otherwise the
// single-container microVM path.
func Run(args Args) error {
	if args.Pod != "" {
		return RunPod(args)
	}
	return runMicroVM(args)
}

// runMicroVM orchestrates the host-side prep and then dials weft's
// gRPC API directly (same library wire weft uses) to register and
// start the microVM. Going in-process keeps the consumer a single
// Go binary — no `weft` subprocess, no third-party tool.
//
// Flow:
//
//  1. Verify the image is pulled (rootfs/ present).
//  2. Apply per-run command overrides to <rootfs>/.weft-microvm/config.json.
//  3. Locate weft-microvm-init boot artefacts (env var or default cache path).
//  4. Dial weft's Unix socket at $XDG_DATA_HOME/.weft/weft.sock
//     (default ~/.weft/weft.sock — same as weft).
//  5. RegisterMicroVM(name=weft-microvm-<refsafe>, boot artefacts,
//     shares=[{tag: "rootfs0", path: <rootfs>}])
//  6. StartVM(name=weft-microvm-<refsafe>).
//
// The VM ends up in weft's inventory and is controllable through
// any other client — `weft instance status weft-microvm-<refsafe>`,
// `weft instance stop …`, etc. Same gRPC, just two consumers.
func runMicroVM(a Args) error {
	rs := refsafe(a.Image)
	rootfs := rootfsPath(rs)

	// Auto-pull on cache miss: matches `docker run`'s UX (pull
	// happens transparently the first time, subsequent runs are
	// instant). Set WEFT_NO_AUTO_PULL=1 to disable when you want
	// strict offline mode and prefer a hard error.
	if _, err := os.Stat(rootfs); err != nil {
		if os.Getenv("WEFT_NO_AUTO_PULL") == "1" {
			return fmt.Errorf("image not pulled and WEFT_NO_AUTO_PULL=1 — run `weft-microvm pull %s` first (missing rootfs at %s)", a.Image, rootfs)
		}
		fmt.Fprintf(os.Stderr, "weft-microvm: image not in cache, auto-pulling %s …\n", a.Image)
		if err := Pull(a.Image); err != nil {
			return fmt.Errorf("auto-pull %s: %w", a.Image, err)
		}
		// Pull populates the same path we just checked.
	}
	if err := applyRunOverrides(rootfs, a); err != nil {
		return err
	}
	iso, err := locateBootArtefacts()
	if err != nil {
		return err
	}

	tag := a.MountTag
	if tag == "" {
		tag = "rootfs0"
	}
	vmName := "weft-microvm-" + rs

	// Prepare the RegisterMicroVMRequest. Two boot modes:
	//   * direct-Linux: kernel + optional initrd + cmdline — fastest
	//     cold-boot, no UKI/ISO assembly needed. Default here.
	//   * UKI: a pre-built boot.iso. Used when the operator points
	//     WEFT_INIT_ISO at one, or when the locateBootArtefacts
	//     cascade finds an iso but no kernel.
	// The cmdline default tells weft-microvm-init which share carries the
	// rootfs; overrides (Args.MountTag) flow through.
	// Clone:true asks weft to materialise an APFS clonefile(2) copy
	// of the rootfs into <vmDir>/<tag>/ before exposing it. Each VM
	// instance gets a private writable view; the shared image cache
	// at `rootfs` stays untouched no matter how many runs land on
	// the same OCI ref. clonefile shares blocks until first write,
	// so the cost is paid only on divergence.
	req := &weftv1.RegisterMicroVMRequest{
		Name:    vmName,
		Project: a.Project,
		Shares: []*weftv1.MicroVMShare{
			{Tag: tag, Path: rootfs, ReadOnly: false, Clone: true},
		},
		Cmdline: fmt.Sprintf("weft.rootfs=virtiofs:%s console=hvc0", tag),
	}
	if iso.kernel != "" {
		req.Kernel = iso.kernel
		req.Initrd = iso.initrd
	} else {
		req.BootIso = iso.bootISO
	}

	client, conn, err := dialWeft(a.WeftSocket)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Fprintf(os.Stderr, "weft-microvm: RegisterMicroVM name=%s boot=%s share=%s=%s cmdline=%q\n", vmName, iso.describe(), tag, rootfs, req.Cmdline)
	if _, err := client.RegisterMicroVM(ctx, req); err != nil {
		return fmt.Errorf("weft RegisterMicroVM: %w", err)
	}

	fmt.Fprintf(os.Stderr, "weft-microvm: StartVM name=%s\n", vmName)
	if _, err := client.StartVM(ctx, &weftv1.StartVMRequest{Name: vmName}); err != nil {
		return fmt.Errorf("weft StartVM: %w", err)
	}

	fmt.Fprintf(os.Stderr, "weft-microvm: %s started — use `weft instance status %s` for status\n", vmName, vmName)
	return nil
}

// dialWeft opens a gRPC connection to weft's Unix socket. Mirrors
// weft/shared.Dial's no-SSH path: same socket convention, same
// insecure transport (Unix socket on the same host — no transit
// security needed). Default socket path matches weft's default
// (`~/.weft/weft.sock`) so a single running daemon serves both
// clients.
func dialWeft(socketPath string) (weftv1.WeftAgentClient, *grpc.ClientConn, error) {
	return weftclient.Client(socketPath)
}

// applyRunOverrides rewrites <rootfs>/.weft-microvm/config.json when the
// caller asked for a command override (Args.Cmd set). The pull step
// writes a config derived from the image config; this step layers
// the per-run overrides on top so the guest's weft-microvm-init picks up
// exactly what the operator asked for.
func applyRunOverrides(rootfs string, a Args) error {
	if len(a.Cmd) == 0 {
		// Nothing to override; the image-derived config stands.
		return nil
	}
	cfgPath := filepath.Join(rootfs, ".weft-microvm", "config.json")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read .weft-microvm/config.json: %w", err)
	}
	var cf configFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return fmt.Errorf("decode .weft-microvm/config.json: %w", err)
	}
	updated := applyUserOverrides(cf.Process, a)
	out, err := marshalConfig(updated)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		return fmt.Errorf("write .weft-microvm/config.json: %w", err)
	}
	return nil
}

// bootArtefacts is the resolved set of files runMicroVM hands to weft
// via RegisterMicroVMRequest. Exactly one of:
//   - bootISO set (UKI mode), or
//   - kernel set (+ optional initrd; direct-Linux mode)
//
// is populated.
type bootArtefacts struct {
	bootISO string // set in UKI mode
	kernel  string // set in direct-Linux mode
	initrd  string // optional, direct-Linux mode
}

func (b bootArtefacts) describe() string {
	if b.bootISO != "" {
		return "iso=" + b.bootISO
	}
	if b.initrd != "" {
		return "kernel=" + b.kernel + " initrd=" + b.initrd
	}
	return "kernel=" + b.kernel
}

// locateBootArtefacts resolves which boot mode + files runMicroVM
// will use. Resolution order:
//
//  1. WEFT_KERNEL env var (with optional WEFT_INITRD) — direct-Linux.
//  2. $XDG_DATA_HOME/weft-microvm/kernel (+ initrd if present) — direct-Linux.
//  3. WEFT_INIT_ISO env var — UKI mode.
//  4. $XDG_DATA_HOME/weft-microvm/weft-microvm-init.iso — UKI mode.
//
// Direct-Linux mode is preferred when available (faster cold-boot,
// no UKI/ISO assembly needed). Returns an actionable error when
// nothing is found.
func locateBootArtefacts() (bootArtefacts, error) {
	// Env-overridden direct-Linux.
	if k := os.Getenv("WEFT_KERNEL"); k != "" {
		if _, err := os.Stat(k); err == nil {
			return bootArtefacts{kernel: k, initrd: os.Getenv("WEFT_INITRD")}, nil
		}
	}
	// Default direct-Linux paths.
	defKernel := filepath.Join(dataDir(), "kernel")
	defInitrd := filepath.Join(dataDir(), "initrd")
	if _, err := os.Stat(defKernel); err == nil {
		out := bootArtefacts{kernel: defKernel}
		if _, err := os.Stat(defInitrd); err == nil {
			out.initrd = defInitrd
		}
		return out, nil
	}
	// Env-overridden UKI.
	if i := os.Getenv("WEFT_INIT_ISO"); i != "" {
		if _, err := os.Stat(i); err == nil {
			return bootArtefacts{bootISO: i}, nil
		}
	}
	// Default UKI path.
	defISO := filepath.Join(dataDir(), "weft-microvm-init.iso")
	if _, err := os.Stat(defISO); err == nil {
		return bootArtefacts{bootISO: defISO}, nil
	}
	return bootArtefacts{}, fmt.Errorf(
		"no weft-microvm boot artefacts found — looked at:\n"+
			"  $WEFT_KERNEL          (+ $WEFT_INITRD)\n"+
			"  %s   (+ initrd if present)\n"+
			"  $WEFT_INIT_ISO\n"+
			"  %s\n"+
			"\n"+
			"build them from this repo's init/ (see weft-microvm/README.md);\n"+
			"direct-Linux mode (kernel + initramfs) is preferred — faster cold-boot,\n"+
			"no UKI/ISO assembly required",
		defKernel, defISO,
	)
}
