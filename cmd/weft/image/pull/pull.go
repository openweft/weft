// Package pull implements the weft pull sub-command.
package pull

import (
	"context"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the pull cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var cfgDir string
	var parallel int
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Pull images defined in the HCL config (via weft)",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.PullImages(context.Background(), &weftv1.PullImagesRequest{
				ConfigDir: cfgDir,
				Parallel:  int32(parallel),
			})
			return err
		},
	}
	cmd.Flags().StringVar(&cfgDir, "config-dir", "state/hcl", "Path to HCL config directory")
	cmd.Flags().IntVar(&parallel, "parallel", 4, "Parallelism for pulls")
	return cmd
}
