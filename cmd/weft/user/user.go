// Package user implements the `weft user` subcommand group: CRUD
// over weft's UUID-keyed user registry. Authoritative
// counterpart to `weft whoami` (which decodes the local id_token):
//
//	weft user ls                                 (platform-admin only)
//	weft user get <UUID>                         (self or admin)
//	weft user me                                 (every caller — refreshes last_seen)
//	weft user set-display-name <UUID> "<name>"   (self or admin)
//	weft user rm <UUID>                          (admin only)
package user

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage authenticated users (UUID-keyed, OIDC-backed)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		getCmd(socket, sshSocket, sshKey),
		meCmd(socket, sshSocket, sshKey),
		setDisplayNameCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List all users (platform-admin only)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListUsers(context.Background(), &weftv1.ListUsersRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Users)
			}
			return renderTable(resp.Users)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func getCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "get <UUID>",
		Short: "Fetch one user (self or platform-admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetUser(context.Background(), &weftv1.GetUserRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON([]*weftv1.UserInfo{resp.User})
			}
			return renderTable([]*weftv1.UserInfo{resp.User})
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func meCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "me",
		Short: "Print the authenticated caller's user record (refreshes last_seen)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.Me(context.Background(), &weftv1.MeRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON([]*weftv1.UserInfo{resp.User})
			}
			return renderTable([]*weftv1.UserInfo{resp.User})
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func setDisplayNameCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set-display-name <UUID> <display-name>",
		Short: "Update the operator-friendly name (self or admin)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetUserDisplayName(context.Background(), &weftv1.SetUserDisplayNameRequest{
				Uuid: args[0], DisplayName: args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("display-name\t%s\t%s\n", resp.User.Uuid, resp.User.DisplayName)
			return nil
		},
	}
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <UUID>",
		Short: "Delete a user (admin only; does NOT cascade onto project ACLs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteUser(context.Background(), &weftv1.DeleteUserRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
}

func renderTable(users []*weftv1.UserInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tISSUER\tSUBJECT\tEMAIL\tDISPLAY_NAME\tGROUPS\tLAST_SEEN")
	for _, u := range users {
		groups := strings.Join(u.Groups, ",")
		if groups == "" {
			groups = "-"
		}
		display := u.DisplayName
		if display == "" {
			display = "-"
		}
		email := u.Email
		if email == "" {
			email = "-"
		}
		seen := "-"
		if u.LastSeenAtUnixNs > 0 {
			seen = time.Unix(0, u.LastSeenAtUnixNs).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			u.Uuid, u.OidcIssuer, u.OidcSubject, email, display, groups, seen)
	}
	return tw.Flush()
}

func dumpJSON(users []*weftv1.UserInfo) error {
	type out struct {
		UUID        string   `json:"uuid"`
		Issuer      string   `json:"oidc_issuer"`
		Subject     string   `json:"oidc_subject"`
		Email       string   `json:"email,omitempty"`
		DisplayName string   `json:"display_name,omitempty"`
		Groups      []string `json:"groups,omitempty"`
		CreatedAt   string   `json:"created_at"`
		LastSeenAt  string   `json:"last_seen_at,omitempty"`
	}
	flat := make([]out, len(users))
	for i, u := range users {
		var lastSeen string
		if u.LastSeenAtUnixNs > 0 {
			lastSeen = time.Unix(0, u.LastSeenAtUnixNs).UTC().Format(time.RFC3339Nano)
		}
		flat[i] = out{
			UUID:        u.Uuid,
			Issuer:      u.OidcIssuer,
			Subject:     u.OidcSubject,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			Groups:      u.Groups,
			CreatedAt:   time.Unix(0, u.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
			LastSeenAt:  lastSeen,
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
