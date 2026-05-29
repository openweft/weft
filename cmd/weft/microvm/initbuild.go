// initbuild.go implements `weft microvm init-build` — packs a Linux
// ncl-init ELF into an initramfs cpio.gz the microVM boot path loads.
// Delegates the packing to the shared weft-microvm/initbuild library;
// this file only resolves the default output location.
package microvm

import (
	"os"
	"path/filepath"

	"github.com/openweft/weft-microvm/initbuild"
	"github.com/spf13/cobra"
)

// initBuildCmd returns the `weft microvm init-build` command.
func initBuildCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "init-build INIT_BINARY",
		Short: "Pack a Linux ncl-init binary into an initramfs cpio.gz",
		Long: `Packs a Linux ncl-init ELF into an initramfs cpio.gz the microVM
boot path loads. Cross-compile the binary first, e.g.:

	GOOS=linux GOARCH=arm64 go build -o /tmp/ncl-init ./cmd/ncl-init`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dst := out
			if dst == "" {
				dst = defaultInitrdPath()
			}
			return initbuild.PackToFile(args[0], dst)
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "output path for the initramfs cpio.gz (default $XDG_DATA_HOME/ncl/initrd)")
	return cmd
}

// defaultInitrdPath returns where the microVM boot path looks for the
// initramfs by default. Mirrors the original ncl runner's
// defaultInitrdPath so both front-ends agree on the cache location.
func defaultInitrdPath() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "ncl", "initrd")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "share", "ncl", "initrd")
	}
	return "/tmp/ncl-initrd"
}
