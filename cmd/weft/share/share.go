// Package share implements `weft share attach/detach`: fan a CubeFS share
// mount (or unmount) out to every VM in a project over the event bus. The
// daemon resolves the project's VMs and publishes per-VM; the in-VM agent
// applies it idempotently.
package share

import (
	"context"
	"fmt"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/spf13/cobra"
)

// Command returns the `share` parent command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Attach or detach CubeFS shares across a project's micro-VMs",
	}
	cmd.AddCommand(
		attachCmd(socket, sshSocket, sshKey),
		detachCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func attachCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, id, mountPoint, volume, owner, accessKey, secretKey, subdir string
	var masters []string
	var readonly bool

	c := &cobra.Command{
		Use:   "attach",
		Short: "Mount a CubeFS share on every VM in a project",
		RunE: func(_ *cobra.Command, _ []string) error {
			cl, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := cl.PublishShareToProject(context.Background(), &weftv1.PublishShareToProjectRequest{
				ProjectUuid: project,
				Mount: &weftv1.ShareMount{
					Id:         id,
					MountPoint: mountPoint,
					Readonly:   readonly,
					Cubefs: &weftv1.CubeFSMount{
						Volume:    volume,
						Masters:   masters,
						Owner:     owner,
						AccessKey: accessKey,
						SecretKey: secretKey,
						Subdir:    subdir,
					},
				},
			})
			if err != nil {
				return err
			}
			fmt.Printf("share %q attached to %d VM(s) in project %s\n", id, resp.VmCount, project)
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&project, "project", "", "project (name or UUID) whose VMs mount the share")
	f.StringVar(&id, "id", "", "stable mount id (use the same id to detach later)")
	f.StringVar(&mountPoint, "mount-point", "", "guest path to mount at, e.g. /data")
	f.StringVar(&volume, "volume", "", "CubeFS volume name")
	f.StringSliceVar(&masters, "masters", nil, "CubeFS master addresses (host:port, comma-separated)")
	f.StringVar(&owner, "owner", "", "CubeFS volume owner")
	f.StringVar(&accessKey, "access-key", "", "CubeFS access key")
	f.StringVar(&secretKey, "secret-key", "", "CubeFS secret key")
	f.StringVar(&subdir, "subdir", "", "subdirectory of the volume to mount")
	f.BoolVar(&readonly, "readonly", false, "mount read-only")
	for _, r := range []string{"project", "id", "mount-point", "volume", "owner", "masters"} {
		_ = c.MarkFlagRequired(r)
	}
	return c
}

func detachCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, id, mountPoint string

	c := &cobra.Command{
		Use:   "detach",
		Short: "Unmount a previously-attached share from a project's VMs",
		RunE: func(_ *cobra.Command, _ []string) error {
			cl, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := cl.PublishShareToProject(context.Background(), &weftv1.PublishShareToProjectRequest{
				ProjectUuid: project,
				Mount: &weftv1.ShareMount{
					Id:         id,
					Action:     "unmount",
					MountPoint: mountPoint,
				},
			})
			if err != nil {
				return err
			}
			fmt.Printf("share %q detached from %d VM(s) in project %s\n", id, resp.VmCount, project)
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&project, "project", "", "project (name or UUID)")
	f.StringVar(&id, "id", "", "mount id used at attach time")
	f.StringVar(&mountPoint, "mount-point", "", "guest mount path used at attach time")
	for _, r := range []string{"project", "id", "mount-point"} {
		_ = c.MarkFlagRequired(r)
	}
	return c
}
