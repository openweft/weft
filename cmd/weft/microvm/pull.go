// pull.go implements `weft microvm pull` — pre-pulls an OCI image and
// extracts its rootfs into the local cache so a subsequent `run` is
// instant. Delegates entirely to the shared weft-microvm library.
package microvm

import (
	"github.com/openweft/weft-microvm"
	"github.com/spf13/cobra"
)

// pullCmd returns the `weft microvm pull` command.
func pullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull IMAGE[:TAG]",
		Short: "Pull an OCI image and extract its rootfs into the cache",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return microvm.Pull(args[0])
		},
	}
}
