// podinitbuild.go implements `weft microvm pod-init-build` — packs a
// pod-mode initramfs: /init (weft-init, the supervisor PID 1) plus the
// guest helper binaries at bin/<name> (crun, cfs-client, the in-VM agent).
// The guest resolves them from $PATH (/bin) — no go:embed, no extraction.
package microvm

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/openweft/weft-microvm/initbuild"
	"github.com/spf13/cobra"
)

// podInitBuildCmd returns the `weft microvm pod-init-build` command.
func podInitBuildCmd() *cobra.Command {
	var initBin, crunBin, cfsBin, agentBin, out string
	cmd := &cobra.Command{
		Use:   "pod-init-build",
		Short: "Pack a pod-mode initramfs (weft-init + crun + cfs-client + agent)",
		Long: `Packs weft-init plus the guest helper binaries into one initramfs
cpio.gz the pod boot path loads. Cross-compile each for the guest arch
first, e.g.:

    GOOS=linux GOARCH=arm64 go build -o /tmp/weft-init \
        github.com/openweft/weft-microvm-init/cmd/weft-init
    task build-crun        # crun-build/dist/crun.linux.arm64 (in weft-microvm-init)

Helper paths are optional — omit one to leave it out of the image.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if initBin == "" {
				return fmt.Errorf("--init is required")
			}
			dst := out
			if dst == "" {
				dst = defaultPodInitrdPath()
			}
			if err := initbuild.PodInitrd(dst, initBin, crunBin, cfsBin, agentBin); err != nil {
				return err
			}
			fmt.Printf("pod initramfs written to %s\n", dst)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&initBin, "init", "", "path to the Linux weft-init ELF (required)")
	f.StringVar(&crunBin, "crun", "", "path to the Linux crun binary -> bin/crun")
	f.StringVar(&cfsBin, "cfs-client", "", "path to the Linux cfs-client binary -> bin/cfs-client")
	f.StringVar(&agentBin, "agent", "", "path to the Linux weft-microvm-agent binary -> bin/weft-microvm-agent")
	f.StringVarP(&out, "output", "o", "", "output path (default $XDG_DATA_HOME/weft-microvm/pod-initrd)")
	return cmd
}

// defaultPodInitrdPath mirrors weft-microvm's locatePodBoot default so the
// builder and the boot path agree on where the pod initramfs lives.
func defaultPodInitrdPath() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "weft-microvm", "pod-initrd")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "share", "weft-microvm", "pod-initrd")
	}
	return "/tmp/weft-pod-initrd"
}
