// pull_kernel.go implements `weft microvm pull-kernel` — fetches the shared
// microVM kernel binary from an OCI artifact (custom mediatype, built by the
// openweft/weft-microvm-kernel CI workflow) and writes it into the local
// weft-microvm data dir so the agent's RegisterMicroVM can pick it up.
//
// Sibling of `weft microvm pull <image>` (which pulls a rootfs for a service
// VM); same OCI client, same shape, different layer media type.
package microvm

import (
	"github.com/openweft/weft-microvm"
	"github.com/spf13/cobra"
)

// pullKernelCmd returns the `weft microvm pull-kernel` command.
func pullKernelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull-kernel REF",
		Short: "Pull the shared microVM kernel binary from an OCI artifact",
		Long: `Fetches the microVM kernel from an OCI artifact (produced by the
openweft/weft-microvm-kernel CI workflow) and writes it to
$XDG_DATA_HOME/weft-microvm/kernel — the path the agent expects.

The artifact's kernel-layer media type is
application/vnd.openweft.microvm.kernel.image.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return microvm.PullKernel(args[0])
		},
	}
}
