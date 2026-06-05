// Package sshkeycatalogue implements the `weft sshkey-catalogue`
// subcommand group — the cluster-wide named-key catalogue VMs
// reference by name at CreateVM time. Distinct from the per-VM
// `weft instance sshkey` surface.
//
//	weft sshkey-catalogue ls
//	weft sshkey-catalogue add    --name N --public-key K [--comment C]
//	weft sshkey-catalogue rm     <uuid|name>
//	weft sshkey-catalogue import --name-prefix P --file F
package sshkeycatalogue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/spf13/cobra"
)

// Command returns the `weft sshkey-catalogue` parent command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sshkey-catalogue",
		Aliases: []string{"sshkey-cat"},
		Short:   "Manage the cluster-wide named SSH-key catalogue",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		addCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
		importCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List cluster catalogue entries",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListSSHKeyCatalogue(context.Background(), &weftv1.ListSSHKeyCatalogueRequest{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UUID\tNAME\tFINGERPRINT\tCOMMENT\tADDED")
			for _, k := range resp.Keys {
				added := "-"
				if k.AddedAtUnixNs != 0 {
					added = time.Unix(0, k.AddedAtUnixNs).UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.Uuid, k.Name, k.Fingerprint, dashIf(k.Comment), added)
			}
			return tw.Flush()
		},
	}
}

func addCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var name, publicKey, comment string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add (or upsert by fingerprint) one named key (admin-only)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			body := publicKey
			if strings.HasPrefix(body, "@") {
				b, err := os.ReadFile(body[1:])
				if err != nil {
					return fmt.Errorf("read public-key file: %w", err)
				}
				body = strings.TrimSpace(string(b))
			}
			resp, err := c.AddSSHKeyCatalogue(context.Background(), &weftv1.AddSSHKeyCatalogueRequest{
				Name: name, PublicKey: body, Comment: comment,
			})
			if err != nil {
				return err
			}
			fmt.Printf("%s\t%s\t%s\tadded=%v\n", resp.Key.Uuid, resp.Key.Name, resp.Key.Fingerprint, resp.Added)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Unique cluster-wide name (required)")
	cmd.Flags().StringVar(&publicKey, "public-key", "", "OpenSSH single-line key body, or @file (required)")
	cmd.Flags().StringVar(&comment, "comment", "", "Optional comment")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("public-key")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid|name>",
		Short: "Remove one catalogue entry (admin-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			req := &weftv1.RemoveSSHKeyCatalogueRequest{}
			if looksLikeUUID(args[0]) {
				req.Uuid = args[0]
			} else {
				req.Name = args[0]
			}
			resp, err := c.RemoveSSHKeyCatalogue(context.Background(), req)
			if err != nil {
				return err
			}
			fmt.Println(resp.DeletedUuid)
			return nil
		},
	}
}

func importCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var namePrefix, comment, file string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Bulk import an authorized_keys blob (admin-only)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if file == "" {
				return fmt.Errorf("--file is required")
			}
			blob, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("read file: %w", err)
			}
			resp, err := c.ImportSSHKeyCatalogue(context.Background(), &weftv1.ImportSSHKeyCatalogueRequest{
				NamePrefix: namePrefix, Blob: string(blob), Comment: comment,
			})
			if err != nil {
				return err
			}
			fmt.Printf("imported=%d skipped_duplicates=%d\n", len(resp.Imported), resp.SkippedDuplicates)
			for _, k := range resp.Imported {
				fmt.Printf("  %s\t%s\t%s\n", k.Uuid, k.Name, k.Fingerprint)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&namePrefix, "name-prefix", "", "Prefix for the per-line auto-naming (required, e.g. \"alice\")")
	cmd.Flags().StringVar(&file, "file", "", "Path to the authorized_keys blob (required)")
	cmd.Flags().StringVar(&comment, "comment", "", "Optional comment applied to every row")
	_ = cmd.MarkFlagRequired("name-prefix")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	return s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
