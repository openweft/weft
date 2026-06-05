// backup.go implements the `weft volume backup` subcommand group :
// off-host backups of block-backend volumes through one of four
// target types :
//
//	oci://<registry>/<repo>:<tag>       — recommended for openweft (content-
//	                                      addressed, cosign-signable, mirrors
//	                                      cleanly with standard OCI tooling)
//	s3://<bucket>@<region>/<prefix>     — versitygw / CubeFS objectnode
//	sftp://<user>@<host>:<port>/<path>  — sftpgo / OpenSSH sshd
//	fs:///<absolute_path>               — dev / tests, no off-host durability
//
// Encryption-at-rest is enabled at the daemon layer via the
// WEFT_BACKUP_PASSPHRASE env var (or WEFT_BACKUP_PASSPHRASE_ENV
// to point at a different one). The CLI never sees the passphrase
// — see pkg/backupcrypto in weft-block for the AEAD pipeline.
//
//	weft volume backup create  --snapshot=<UUID> --target=<URL> [--project=<p>]
//	weft volume backup list    --target=<URL> [--volume=<UUID>] [--project=<p>] [--format json]
//	weft volume backup delete  --url=<URL>
//	weft volume backup restore --url=<URL> --new-volume=<name> --project=<p>

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

func backupCommand(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Manage off-host volume backups (oci:// / s3:// / sftp:// / fs:// targets)",
	}
	cmd.AddCommand(
		backupCreateCmd(socket, sshSocket, sshKey),
		backupListCmd(socket, sshSocket, sshKey),
		backupDeleteCmd(socket, sshSocket, sshKey),
		backupRestoreCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func backupCreateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var snapshot, target, project string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Ship a snapshot to a backup target (block-backend volumes only)",
		Example: `  # OCI (recommended) :
  weft volume backup create --snapshot=$SNAP --target=oci://ghcr.io/acme/backups:my-vol-2026-06-04
  # versitygw / CubeFS objectnode :
  weft volume backup create --snapshot=$SNAP --target=s3://backups@us-east-1/my-volume/
  # sftpgo :
  weft volume backup create --snapshot=$SNAP --target=sftp://backupbot@backup.example.com:2022/backups/my-volume/`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateVolumeBackup(context.Background(), &weftv1.CreateVolumeBackupRequest{
				SnapshotUuid: snapshot,
				Target:       target,
				Project:      project,
			})
			if err != nil {
				return err
			}
			fmt.Printf("backup\t%s\t%s\t%d bytes\t%s\n",
				resp.Backup.Url, resp.Backup.SnapshotUuid, resp.Backup.SizeBytes, resp.Backup.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "Source snapshot UUID (must already exist)")
	cmd.Flags().StringVar(&target, "target", "", "Backup target URL (oci:// / s3:// / sftp:// / fs://)")
	cmd.Flags().StringVar(&project, "project", "", "Project (display name or UUID; optional)")
	_ = cmd.MarkFlagRequired("snapshot")
	_ = cmd.MarkFlagRequired("target")
	return cmd
}

func backupListCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var target, volume, project, format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List backups at a target (optionally scoped to one volume / project)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListVolumeBackups(context.Background(), &weftv1.ListVolumeBackupsRequest{
				Target:     target,
				VolumeUuid: volume,
				Project:    project,
			})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpBackupsJSON(resp.Backups)
			}
			return renderBackupsTable(resp.Backups)
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Backup target URL (required)")
	cmd.Flags().StringVar(&volume, "volume", "", "Limit to one origin volume (UUID)")
	cmd.Flags().StringVar(&project, "project", "", "Limit to one project (display name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	_ = cmd.MarkFlagRequired("target")
	return cmd
}

func backupDeleteCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var url string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete one backup from its target store (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteVolumeBackup(context.Background(), &weftv1.DeleteVolumeBackupRequest{Url: url}); err != nil {
				return err
			}
			fmt.Println(url)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "Backup URL (as returned by `backup list`) (required)")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}

func backupRestoreCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var url, newVolume, project string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a backup into a fresh block-backend volume",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.RestoreVolumeBackup(context.Background(), &weftv1.RestoreVolumeBackupRequest{
				Url:           url,
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
	cmd.Flags().StringVar(&url, "url", "", "Backup URL to restore (required)")
	cmd.Flags().StringVar(&newVolume, "new-volume", "", "Name for the restored volume (required)")
	cmd.Flags().StringVar(&project, "project", "", "Project the new volume lands in (required)")
	_ = cmd.MarkFlagRequired("url")
	_ = cmd.MarkFlagRequired("new-volume")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

func renderBackupsTable(backups []*weftv1.VolumeBackupInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "URL\tVOLUME_UUID\tSNAPSHOT_UUID\tPROJECT\tSIZE\tSTATE\tCREATED")
	for _, b := range backups {
		created := time.Unix(0, b.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d B\t%s\t%s\n",
			b.Url, b.VolumeUuid, b.SnapshotUuid, b.Project, b.SizeBytes, b.State, created)
	}
	return tw.Flush()
}

func dumpBackupsJSON(backups []*weftv1.VolumeBackupInfo) error {
	type out struct {
		URL          string `json:"url"`
		VolumeUUID   string `json:"volume_uuid"`
		SnapshotUUID string `json:"snapshot_uuid"`
		Project      string `json:"project"`
		SizeBytes    int64  `json:"size_bytes"`
		State        string `json:"state"`
		Error        string `json:"error,omitempty"`
		CreatedAt    string `json:"created_at"`
	}
	flat := make([]out, len(backups))
	for i, b := range backups {
		flat[i] = out{
			URL:          b.Url,
			VolumeUUID:   b.VolumeUuid,
			SnapshotUUID: b.SnapshotUuid,
			Project:      b.Project,
			SizeBytes:    b.SizeBytes,
			State:        b.State,
			Error:        b.Error,
			CreatedAt:    time.Unix(0, b.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
