// Pod mode (Args.Pod set): boot a micro-VM that runs *several* OCI
// containers under weft-init (a supervisor PID 1 from
// openweft/weft-microvm-init), instead of the single-container weft-microvm-init
// path.
//
// The manifest is a small user-facing shape — a pod id plus a list
// of {id, image, optional command/env/...}. We expand it: pull +
// extract each image (reusing Pull, so each rootfs lands in the same
// cache with a derived .weft-microvm/config.json), then emit the
// authoritative on-VM spec (weft-microvm-init/pkg/pod.Spec) as pod.json.
//
// Delivery mirrors weft-microvm-init's cmdline convention: a dedicated
// virtio-fs config share (tag "weftcfg") carries pod.json, and the
// guest cmdline carries `weft.config=virtiofs:weftcfg`. weft-init
// mounts that share early, reads /run/weft/pod.json, then mounts the
// per-container rootfs shares (rootfs0..N) the spec names.

package microvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	weftpod "github.com/openweft/weft-microvm-init/pkg/pod"
)

// configTag is the virtio-fs tag of the share that carries pod.json.
const configTag = "weftcfg"

// podManifest is the user-facing pod input. Deliberately smaller
// than weft pod.Spec: we derive rootfs tags, shares, and (when a
// container omits them) command/env/cwd/user from the pulled image
// config.
type podManifest struct {
	PodID      string           `json:"pod_id"`
	Containers []manifestCtr    `json:"containers"`
	Network    *weftpod.Network `json:"network,omitempty"`
}

type manifestCtr struct {
	ID      string                `json:"id"`
	Image   string                `json:"image"`
	Command []string              `json:"command,omitempty"`
	Env     map[string]string     `json:"env,omitempty"`
	Workdir string                `json:"workdir,omitempty"`
	User    string                `json:"user,omitempty"`
	Restart weftpod.RestartPolicy `json:"restart,omitempty"`
}

// RunPod expands the manifest at Args.Pod, builds the weft pod.Spec +
// virtio-fs shares, writes pod.json into a config share, and
// registers/starts the micro-VM with weft-init as PID 1. It is the
// pod-mode branch dispatched from Run.
func RunPod(a Args) error {
	man, err := loadManifest(a.Pod)
	if err != nil {
		return err
	}

	spec := weftpod.Spec{PodID: man.PodID, Network: man.Network}
	shares := []*weftv1.MicroVMShare{} // populated with one rootfs share per container

	for i := range man.Containers {
		c := &man.Containers[i]
		tag := fmt.Sprintf("rootfs%d", i)

		// Pull on cache miss (same auto-pull UX as single-container).
		rs := refsafe(c.Image)
		rootfs := rootfsPath(rs)
		if _, err := os.Stat(rootfs); err != nil {
			if os.Getenv("WEFT_NO_AUTO_PULL") == "1" {
				return fmt.Errorf("container %q: image %q not pulled and WEFT_NO_AUTO_PULL=1 (missing %s)", c.ID, c.Image, rootfs)
			}
			fmt.Fprintf(os.Stderr, "weft-microvm: pulling %s for container %q …\n", c.Image, c.ID)
			if err := Pull(c.Image); err != nil {
				return fmt.Errorf("container %q: pull %s: %w", c.ID, c.Image, err)
			}
		}

		ctr, err := containerFromImage(c, tag, rootfs)
		if err != nil {
			return fmt.Errorf("container %q: %w", c.ID, err)
		}
		spec.Containers = append(spec.Containers, ctr)
		spec.Shares = append(spec.Shares, weftpod.Share{
			Tag:        tag,
			MountPoint: "/run/weft/rootfs/" + c.ID,
		})
		shares = append(shares, &weftv1.MicroVMShare{
			Tag: tag, Path: rootfs, ReadOnly: false, Clone: true,
		})
	}

	if err := spec.Validate(); err != nil {
		return fmt.Errorf("pod spec: %w", err)
	}

	cfgShare, err := writePodJSON(man.PodID, spec)
	if err != nil {
		return err
	}
	// The config share is read-only and prepended so weft-init can
	// mount it before the rootfs shares.
	shares = append([]*weftv1.MicroVMShare{
		{Tag: configTag, Path: cfgShare, ReadOnly: true, Clone: false},
	}, shares...)

	kernel, initrd, err := locatePodBoot()
	if err != nil {
		return err
	}

	vmName := "weft-microvm-pod-" + man.PodID
	req := &weftv1.RegisterMicroVMRequest{
		Name:    vmName,
		Project: a.Project,
		Shares:  shares,
		Kernel:  kernel,
		Initrd:  initrd,
		Cmdline: fmt.Sprintf("weft.config=virtiofs:%s console=hvc0", configTag),
	}

	client, conn, err := dialWeft(a.WeftSocket)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Fprintf(os.Stderr, "weft-microvm: RegisterMicroVM name=%s containers=%d kernel=%s initrd=%s\n",
		vmName, len(spec.Containers), kernel, initrd)
	if _, err := client.RegisterMicroVM(ctx, req); err != nil {
		return fmt.Errorf("weft RegisterMicroVM: %w", err)
	}
	fmt.Fprintf(os.Stderr, "weft-microvm: StartVM name=%s\n", vmName)
	if _, err := client.StartVM(ctx, &weftv1.StartVMRequest{Name: vmName}); err != nil {
		return fmt.Errorf("weft StartVM: %w", err)
	}
	fmt.Fprintf(os.Stderr, "weft-microvm: %s started — `weft instance status %s` for status\n", vmName, vmName)
	return nil
}

func loadManifest(path string) (*podManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pod manifest %s: %w", path, err)
	}
	var m podManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode pod manifest %s: %w", path, err)
	}
	if m.PodID == "" {
		return nil, fmt.Errorf("pod manifest %s: pod_id is required", path)
	}
	if len(m.Containers) == 0 {
		return nil, fmt.Errorf("pod manifest %s: at least one container is required", path)
	}
	for i, c := range m.Containers {
		if c.ID == "" {
			return nil, fmt.Errorf("pod manifest %s: container[%d] missing id", path, i)
		}
		if c.Image == "" {
			return nil, fmt.Errorf("pod manifest %s: container %q missing image", path, c.ID)
		}
	}
	return &m, nil
}

// containerFromImage maps a manifest container onto a weft pod
// container, filling command/env/cwd/user from the image-derived
// <rootfs>/.weft-microvm/config.json unless the manifest overrides them.
func containerFromImage(c *manifestCtr, tag, rootfs string) (weftpod.Container, error) {
	cfgPath := filepath.Join(rootfs, ".weft-microvm", "config.json")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return weftpod.Container{}, fmt.Errorf("read %s: %w", cfgPath, err)
	}
	var cf configFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return weftpod.Container{}, fmt.Errorf("decode %s: %w", cfgPath, err)
	}

	command := c.Command
	if len(command) == 0 {
		command = cf.Process.Args // image Entrypoint+Cmd, already merged
	}
	if len(command) == 0 {
		return weftpod.Container{}, fmt.Errorf("no command: image has no entrypoint/cmd and manifest set none")
	}

	env := envToMap(cf.Process.Env)
	for k, v := range c.Env { // manifest overrides image env
		env[k] = v
	}

	workdir := c.Workdir
	if workdir == "" {
		workdir = cf.Process.Cwd
	}
	user := c.User
	if user == "" && (cf.Process.User.UID != 0 || cf.Process.User.GID != 0) {
		user = fmt.Sprintf("%d:%d", cf.Process.User.UID, cf.Process.User.GID)
	}

	return weftpod.Container{
		ID:        c.ID,
		RootfsTag: tag,
		Command:   command,
		Env:       env,
		Workdir:   workdir,
		User:      user,
		Restart:   c.Restart,
	}, nil
}

func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}

// writePodJSON materialises pod.json in a per-pod config directory
// ($XDG_DATA_HOME/weft-microvm/pods/<id>/) and returns that directory — the
// host path exposed to the guest as the virtio-fs config share.
func writePodJSON(podID string, spec weftpod.Spec) (string, error) {
	dir := filepath.Join(dataDir(), "pods", podID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir pod config dir: %w", err)
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal pod.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pod.json"), b, 0o644); err != nil {
		return "", fmt.Errorf("write pod.json: %w", err)
	}
	return dir, nil
}

// locatePodBoot resolves the kernel + weft-init initramfs for pod
// mode. Pod mode is direct-Linux only (weft-init is an initramfs
// PID 1), so a kernel is required. The initrd is the weft-init
// initramfs, kept separate from weft-microvm-init's.
//
// Resolution:
//
//	kernel: $WEFT_KERNEL, else $XDG_DATA_HOME/weft-microvm/kernel
//	initrd: $WEFT_POD_INITRD, else $XDG_DATA_HOME/weft-microvm/pod-initrd
func locatePodBoot() (kernel, initrd string, err error) {
	kernel = os.Getenv("WEFT_KERNEL")
	if kernel == "" {
		kernel = filepath.Join(dataDir(), "kernel")
	}
	if _, err := os.Stat(kernel); err != nil {
		return "", "", fmt.Errorf("kernel not found at %s (set $WEFT_KERNEL)", kernel)
	}
	initrd = os.Getenv("WEFT_POD_INITRD")
	if initrd == "" {
		initrd = filepath.Join(dataDir(), "pod-initrd")
	}
	if _, err := os.Stat(initrd); err != nil {
		return "", "", fmt.Errorf(
			"weft-init initramfs not found at %s (set $WEFT_POD_INITRD)\n"+
				"build it:\n"+
				"  GOOS=linux GOARCH=<arch> CGO_ENABLED=0 go build -o weft-init github.com/openweft/weft-microvm-init/cmd/weft-init\n"+
				"  weft microvm pod-init-build --init weft-init [--crun … --cfs-client … --agent …] -o %s",
			initrd, initrd)
	}
	return kernel, initrd, nil
}
