// Package share implements the `weft share` subcommand group : both the
// CRUD registry surface (ListShares / CreateShare / DeleteShare) and the
// per-project mount fan-out (attach / detach over PublishShareToProject).
//
//	weft share ls     [--project P] [--format json]
//	weft share show   <uuid>
//	weft share create --project P --name N --size-gb N [--readonly] [--backend cubefs]
//	weft share rm     <uuid>
//	weft share attach --project P --id ... --mount-point ... --volume ... ...
//	weft share detach --project P --id ... --mount-point ...
//
// The `show` verb is a client-side filter over ListShares ; there's no
// GetShare RPC yet (follow-up). Attach/detach drive PublishShareToProject :
// the daemon resolves the project's VMs and the in-VM agent applies the
// mount idempotently.
package share

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/spf13/cobra"
)

// Command returns the `share` parent command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Manage CubeFS share registry and fan-out per-project mounts",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		showCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
		attachCmd(socket, sshSocket, sshKey),
		detachCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List shares (optionally scoped to one project)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cl, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := cl.ListShares(context.Background(), &weftv1.ListSharesRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Shares)
			}
			return renderTable(resp.Shares)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Limit to one project (name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "show <uuid>",
		Short: "Show one share by UUID (client-side filter over ListShares ; no GetShare RPC yet)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cl, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := cl.ListShares(context.Background(), &weftv1.ListSharesRequest{})
			if err != nil {
				return err
			}
			for _, s := range resp.Shares {
				if s.Uuid == args[0] {
					if format == "json" {
						return dumpJSON([]*weftv1.ShareInfo{s})
					}
					return renderTable([]*weftv1.ShareInfo{s})
				}
			}
			return fmt.Errorf("no share with uuid %q", args[0])
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, name, backend string
	var sizeGB int64
	var readonly bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Register a new share (returns its UUID + provisioning status)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cl, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := cl.CreateShare(context.Background(), &weftv1.CreateShareRequest{
				Project:  project,
				Name:     name,
				SizeGb:   sizeGB,
				Readonly: readonly,
				Backend:  backend,
			})
			if err != nil {
				return err
			}
			s := resp.Share
			fmt.Printf("created\t%s\t%s\t%d GB\t%s\t%s\n",
				s.Uuid, s.Name, s.SizeGb, dashIf(s.Backend), s.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (name or UUID, required)")
	cmd.Flags().StringVar(&name, "name", "", "Share name (unique within the project, required)")
	cmd.Flags().Int64Var(&sizeGB, "size-gb", 0, "Quota in GB (required, must be > 0)")
	cmd.Flags().BoolVar(&readonly, "readonly", false, "Provision as read-only")
	cmd.Flags().StringVar(&backend, "backend", "", `Storage backend (empty = "cubefs")`)
	_ = cmd.MarkFlagRequired("project")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("size-gb")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Delete a share registry entry by UUID",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cl, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := cl.DeleteShare(context.Background(), &weftv1.DeleteShareRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
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

func renderTable(shares []*weftv1.ShareInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tPROJECT_UUID\tNAME\tSIZE\tBACKEND\tREADONLY\tMOUNTS\tSTATUS\tCREATED")
	for _, s := range shares {
		created := "-"
		if s.CreatedAtUnixNs != 0 {
			created = time.Unix(0, s.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d GB\t%s\t%t\t%d\t%s\t%s\n",
			s.Uuid, s.ProjectUuid, s.Name, s.SizeGb, dashIf(s.Backend), s.Readonly, s.Mounts, s.Status, created)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpJSON(shares []*weftv1.ShareInfo) error {
	type out struct {
		UUID        string `json:"uuid"`
		ProjectUUID string `json:"project_uuid"`
		Name        string `json:"name"`
		Backend     string `json:"backend"`
		SizeGB      int64  `json:"size_gb"`
		Readonly    bool   `json:"readonly"`
		Mounts      int32  `json:"mounts"`
		Status      string `json:"status"`
		CreatedAt   string `json:"created_at,omitempty"`
	}
	flat := make([]out, len(shares))
	for i, s := range shares {
		created := ""
		if s.CreatedAtUnixNs != 0 {
			created = time.Unix(0, s.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano)
		}
		flat[i] = out{s.Uuid, s.ProjectUuid, s.Name, s.Backend, s.SizeGb, s.Readonly, s.Mounts, s.Status, created}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
