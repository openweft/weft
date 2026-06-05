// Package registry implements the `weft registry` subcommand group —
// OCI registry alias catalogue. Each row maps a human-friendly name
// ("ghcr", "mirror") to the endpoint + insecure flag + credential
// secret ref the OCI puller consults at pull time.
//
//	weft registry ls
//	weft registry set    --name N --endpoint E [--insecure --credential-secret-ref R]
//	weft registry rm     <uuid|name>
//	weft registry search <uuid|name> <query>
package registry

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/spf13/cobra"
)

// Command returns the `weft registry` parent command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage upstream OCI registry catalogue (alias → endpoint)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		setCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
		searchCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List registry aliases",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListRegistryRemotes(context.Background(), &weftv1.ListRegistryRemotesRequest{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UUID\tNAME\tENDPOINT\tINSECURE\tCREDENTIAL_REF\tCREATED")
			for _, r := range resp.Remotes {
				created := "-"
				if r.CreatedAtUnixNs != 0 {
					created = time.Unix(0, r.CreatedAtUnixNs).UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t%s\n", r.Uuid, r.Name, r.Endpoint, r.Insecure, dashIf(r.CredentialSecretRef), created)
			}
			return tw.Flush()
		},
	}
}

func setCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var name, endpoint, credRef string
	var insecure bool
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Upsert a registry alias (admin-only)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetRegistryRemote(context.Background(), &weftv1.SetRegistryRemoteRequest{
				Name: name, Endpoint: endpoint, Insecure: insecure, CredentialSecretRef: credRef,
			})
			if err != nil {
				return err
			}
			verb := "updated"
			if resp.Created {
				verb = "created"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", verb, resp.Remote.Uuid, resp.Remote.Name, resp.Remote.Endpoint)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Alias name (unique cluster-wide, required)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Registry endpoint (e.g. ghcr.io ; required on create)")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Allow http:// and skip TLS verification")
	cmd.Flags().StringVar(&credRef, "credential-secret-ref", "", "Reference into the secret store")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid|name>",
		Short: "Delete a registry alias (admin-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			req := &weftv1.DeleteRegistryRemoteRequest{}
			if looksLikeUUID(args[0]) {
				req.Uuid = args[0]
			} else {
				req.Name = args[0]
			}
			resp, err := c.DeleteRegistryRemote(context.Background(), req)
			if err != nil {
				return err
			}
			fmt.Println(resp.DeletedUuid)
			return nil
		},
	}
}

func searchCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "search <uuid|name> <query>",
		Short: "Search the upstream registry catalogue (today : server-side stub)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			req := &weftv1.SearchRegistryRemoteRequest{Query: args[1]}
			if looksLikeUUID(args[0]) {
				req.Uuid = args[0]
			} else {
				req.Name = args[0]
			}
			resp, err := c.SearchRegistryRemote(context.Background(), req)
			if err != nil {
				return err
			}
			if len(resp.Repositories) == 0 {
				fmt.Printf("(no results for query %q on %s)\n", args[1], resp.RegistryName)
				return nil
			}
			for _, r := range resp.Repositories {
				fmt.Println(r)
			}
			return nil
		},
	}
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
