// Package image implements the weft image sub-command group.
package image

import (
	"context"

	"github.com/openweft/weft/cmd/weft/image/pull"
	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the image cobra command with its sub-commands.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage cached images",
	}
	cmd.AddCommand(
		listCmd(socket, sshSocket, sshKey),
		pull.Command(socket, sshSocket, sshKey),
	)
	return cmd
}

func listCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List locally cached images",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListImages(context.Background(), &weftv1.ListImagesRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return shared.PrintImagesJSON(resp.Images)
			}
			shared.RenderImagesTable(resp.Images)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}
