// Package volume implements the `vzc volume` subcommand group:
// CRUD + attach/detach over vzd's UUID-keyed volume registry.
//
//	vzc volume ls [--project NAME-OR-UUID] [--format json]
//	vzc volume create --project P --name N --size-gib N [--format raw|qcow2]
//	vzc volume rename <UUID> <new-name>
//	vzc volume resize <UUID> <new-size-gib>
//	vzc volume attach <UUID> <vm-uuid>
//	vzc volume detach <UUID>
//	vzc volume rm <UUID>
//
// Display-name resolution (Volume name OR UUID) on the *volume*
// itself is a future polish; the registry indexes by
// (project_uuid, name) so we'd need a project context to do it
// safely. For now `vzc volume <verb> <UUID>` is the canonical
// shape — operators can copy a UUID out of `vzc volume ls`.
package volume

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage block volumes (UUID-keyed, project-scoped)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		renameCmd(socket, sshSocket, sshKey),
		resizeCmd(socket, sshSocket, sshKey),
		attachCmd(socket, sshSocket, sshKey),
		detachCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List volumes (optionally scoped to one project)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListVolumes(context.Background(), &vzdv1.ListVolumesRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Volumes)
			}
			return renderTable(resp.Volumes)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Limit to one project (display name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, name, format string
	var sizeGiB int64
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new volume (returns its UUID)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateVolume(context.Background(), &vzdv1.CreateVolumeRequest{
				Project: project,
				Name:    name,
				SizeGib: sizeGiB,
				Format:  format,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created\t%s\t%s\t%d GiB\t%s\n",
				resp.Volume.Uuid, resp.Volume.Name, resp.Volume.SizeGib, resp.Volume.Format)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (display name or UUID; required unless caller has a single default project)")
	cmd.Flags().StringVar(&name, "name", "", "Volume name (unique within the project)")
	cmd.Flags().Int64Var(&sizeGiB, "size-gib", 0, "Size in GiB (required, must be > 0)")
	cmd.Flags().StringVar(&format, "format", "", `On-disk format: "raw" (default) or "qcow2"`)
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("size-gib")
	return cmd
}

func renameCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <UUID> <new-name>",
		Short: "Rename a volume (UUID unchanged)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.RenameVolume(context.Background(), &vzdv1.RenameVolumeRequest{
				Uuid:    args[0],
				NewName: args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("renamed\t%s\t%s\n", resp.Volume.Uuid, resp.Volume.Name)
			return nil
		},
	}
}

func resizeCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "resize <UUID> <new-size-gib>",
		Short: "Grow a volume (shrink is not supported)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			size, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || size <= 0 {
				return fmt.Errorf("new-size-gib must be a positive integer (got %q)", args[1])
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ResizeVolume(context.Background(), &vzdv1.ResizeVolumeRequest{
				Uuid:       args[0],
				NewSizeGib: size,
			})
			if err != nil {
				return err
			}
			fmt.Printf("resized\t%s\t%d GiB\n", resp.Volume.Uuid, resp.Volume.SizeGib)
			return nil
		},
	}
}

func attachCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "attach <UUID> <vm-uuid>",
		Short: "Bind the volume to a VM (refuses if already attached elsewhere)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.AttachVolume(context.Background(), &vzdv1.AttachVolumeRequest{
				Uuid:   args[0],
				VmUuid: args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("attached\t%s\t→\t%s\n", resp.Volume.Uuid, resp.Volume.AttachedToUuid)
			return nil
		},
	}
}

func detachCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "detach <UUID>",
		Short: "Clear the volume's VM binding",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DetachVolume(context.Background(), &vzdv1.DetachVolumeRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Printf("detached\t%s\n", args[0])
			return nil
		},
	}
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <UUID>",
		Short: "Delete a detached volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteVolume(context.Background(), &vzdv1.DeleteVolumeRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
}

func renderTable(volumes []*vzdv1.VolumeInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tPROJECT_UUID\tNAME\tSIZE\tFORMAT\tATTACHED_TO\tCREATED")
	for _, v := range volumes {
		attached := v.AttachedToUuid
		if attached == "" {
			attached = "-"
		}
		created := time.Unix(0, v.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d GiB\t%s\t%s\t%s\n",
			v.Uuid, v.ProjectUuid, v.Name, v.SizeGib, v.Format, attached, created)
	}
	return tw.Flush()
}

func dumpJSON(volumes []*vzdv1.VolumeInfo) error {
	type out struct {
		UUID        string `json:"uuid"`
		ProjectUUID string `json:"project_uuid"`
		Name        string `json:"name"`
		SizeGiB     int64  `json:"size_gib"`
		Format      string `json:"format"`
		AttachedTo  string `json:"attached_to_uuid,omitempty"`
		CreatedAt   string `json:"created_at"`
	}
	flat := make([]out, len(volumes))
	for i, v := range volumes {
		flat[i] = out{
			UUID:        v.Uuid,
			ProjectUUID: v.ProjectUuid,
			Name:        v.Name,
			SizeGiB:     v.SizeGib,
			Format:      v.Format,
			AttachedTo:  v.AttachedToUuid,
			CreatedAt:   time.Unix(0, v.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
