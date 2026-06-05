// Package volumeproperty implements the `weft volume property` subcommand
// group : per-volume host-set KV annotations (mirror of `weft instance
// property` for block volumes).
//
//	weft volume property set <volume-uuid> <key> <value>
//	weft volume property get <volume-uuid> <key>
//	weft volume property delete <volume-uuid> <key>
package volumeproperty

import (
	"context"
	"fmt"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/spf13/cobra"
)

// Command returns the `weft volume property` parent command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "property",
		Short: "Per-volume host-set key/value annotations",
	}
	cmd.AddCommand(
		setCmd(socket, sshSocket, sshKey),
		getCmd(socket, sshSocket, sshKey),
		deleteCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func setCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set <volume-uuid> <key> <value>",
		Short: "Set (or upsert) a volume property (admin-only)",
		Args:  cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetVolumeProperty(context.Background(), &weftv1.SetVolumePropertyRequest{
				VolumeUuid: args[0], Key: args[1], Value: args[2],
			})
			if err != nil {
				return err
			}
			fmt.Printf("set\t%s\t%s\t%s\n", resp.Property.VolumeUuid, resp.Property.Key, resp.Property.Value)
			return nil
		},
	}
}

func getCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get <volume-uuid> <key>",
		Short: "Get one property by key",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetVolumeProperty(context.Background(), &weftv1.GetVolumePropertyRequest{
				VolumeUuid: args[0], Key: args[1],
			})
			if err != nil {
				return err
			}
			fmt.Println(resp.Property.Value)
			return nil
		},
	}
}

func deleteCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <volume-uuid> <key>",
		Short: "Delete a volume property (admin-only)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteVolumeProperty(context.Background(), &weftv1.DeleteVolumePropertyRequest{
				VolumeUuid: args[0], Key: args[1],
			}); err != nil {
				return err
			}
			fmt.Printf("deleted\t%s\t%s\n", args[0], args[1])
			return nil
		},
	}
}
