// snapshot.go implements the `weft volume snapshot` subcommand
// group, mirroring the volume.go pattern over the four
// VolumeSnapshot RPCs introduced in commit b774e198a:
//
//	weft volume snapshot create  --volume=<UUID> --name=<n> [--project=<p>]
//	weft volume snapshot list    [--volume=<UUID>] [--project=<p>] [--format json]
//	weft volume snapshot restore --snapshot=<UUID> --new-volume=<name> [--project=<p>]
//	weft volume snapshot delete  --uuid=<UUID>
//
// The transport is the same weft-client dial that the volume CRUD
// uses (see volume.go), so operators get the same agent → control-
// plane path. Snapshots are project-scoped on the server side; the
// CLI passes --project verbatim and lets the server resolve it.

package volume

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// snapshotCommand returns the `weft volume snapshot` parent and
// its four leaves. Folded into Command() in volume.go so the
// existing `weft volume <verb>` surface is unchanged.
func snapshotCommand(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Manage volume snapshots (reflink-backed point-in-time copies)",
	}
	cmd.AddCommand(
		snapshotCreateCmd(socket, sshSocket, sshKey),
		snapshotListCmd(socket, sshSocket, sshKey),
		snapshotRestoreCmd(socket, sshSocket, sshKey),
		snapshotDeleteCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func snapshotCreateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var volume, name, project string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Snapshot a volume (reflink clone of the current blob)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateVolumeSnapshot(context.Background(), &weftv1.CreateVolumeSnapshotRequest{
				VolumeUuid: volume,
				Name:       name,
				Project:    project,
			})
			if err != nil {
				return err
			}
			fmt.Printf("snapshot\t%s\t%s\t%s\t%d GiB\n",
				resp.Snapshot.Uuid, resp.Snapshot.VolumeUuid, resp.Snapshot.Name, resp.Snapshot.SizeGib)
			return nil
		},
	}
	cmd.Flags().StringVar(&volume, "volume", "", "Parent volume UUID (required)")
	cmd.Flags().StringVar(&name, "name", "", "Snapshot name (unique within the parent volume)")
	cmd.Flags().StringVar(&project, "project", "", "Project (display name or UUID; optional)")
	_ = cmd.MarkFlagRequired("volume")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func snapshotListCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var volume, project, format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List volume snapshots (optionally scoped to one volume / project)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListVolumeSnapshots(context.Background(), &weftv1.ListVolumeSnapshotsRequest{
				VolumeUuid: volume,
				Project:    project,
			})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpSnapshotsJSON(resp.Snapshots)
			}
			return renderSnapshotsTable(resp.Snapshots)
		},
	}
	cmd.Flags().StringVar(&volume, "volume", "", "Limit to one parent volume (UUID)")
	cmd.Flags().StringVar(&project, "project", "", "Limit to one project (display name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func snapshotRestoreCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var snapshot, newVolume, project string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a snapshot into a fresh volume (same project as the snapshot)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.RestoreVolumeSnapshot(context.Background(), &weftv1.RestoreVolumeSnapshotRequest{
				SnapshotUuid:  snapshot,
				NewVolumeName: newVolume,
				Project:       project,
			})
			if err != nil {
				return err
			}
			fmt.Printf("restored\t%s\t%s\t%d GiB\n",
				resp.Volume.Uuid, resp.Volume.Name, resp.Volume.SizeGib)
			return nil
		},
	}
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "Source snapshot UUID (required)")
	cmd.Flags().StringVar(&newVolume, "new-volume", "", "Name for the restored volume (required)")
	cmd.Flags().StringVar(&project, "project", "", "Project (display name or UUID; optional)")
	_ = cmd.MarkFlagRequired("snapshot")
	_ = cmd.MarkFlagRequired("new-volume")
	return cmd
}

func snapshotDeleteCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var uuid string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a snapshot (does not touch the parent volume)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteVolumeSnapshot(context.Background(), &weftv1.DeleteVolumeSnapshotRequest{Uuid: uuid}); err != nil {
				return err
			}
			fmt.Println(uuid)
			return nil
		},
	}
	cmd.Flags().StringVar(&uuid, "uuid", "", "Snapshot UUID to delete (required)")
	_ = cmd.MarkFlagRequired("uuid")
	return cmd
}

func renderSnapshotsTable(snapshots []*weftv1.VolumeSnapshotInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tVOLUME_UUID\tPROJECT\tNAME\tSIZE\tCREATED")
	for _, s := range snapshots {
		created := time.Unix(0, s.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d GiB\t%s\n",
			s.Uuid, s.VolumeUuid, s.Project, s.Name, s.SizeGib, created)
	}
	return tw.Flush()
}

func dumpSnapshotsJSON(snapshots []*weftv1.VolumeSnapshotInfo) error {
	type out struct {
		UUID       string `json:"uuid"`
		VolumeUUID string `json:"volume_uuid"`
		Project    string `json:"project"`
		Name       string `json:"name"`
		SizeGiB    int64  `json:"size_gib"`
		CreatedAt  string `json:"created_at"`
	}
	flat := make([]out, len(snapshots))
	for i, s := range snapshots {
		flat[i] = out{
			UUID:       s.Uuid,
			VolumeUUID: s.VolumeUuid,
			Project:    s.Project,
			Name:       s.Name,
			SizeGiB:    s.SizeGib,
			CreatedAt:  time.Unix(0, s.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
