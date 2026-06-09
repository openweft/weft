// Package clean implements the weft clean sub-command.
package clean

import (
	"context"
	"fmt"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the clean cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var cfgDir string
	var yes bool
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove cached images referenced in the HCL config (via weft)",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CleanImages(context.Background(), &weftv1.CleanImagesRequest{
				ConfigDir: cfgDir,
				DryRun:    !yes,
			})
			if err != nil {
				return err
			}
			if !yes {
				fmt.Println("dry-run — would delete:")
			}
			for _, d := range resp.Deleted {
				fmt.Println(" -", d)
			}
			if len(resp.Deleted) == 0 {
				fmt.Println("nothing to clean")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgDir, "config-dir", "state/hcl", "Path to HCL config directory")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm deletion (default: dry-run)")
	return cmd
}
