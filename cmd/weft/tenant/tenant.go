// Package tenant implements the `weft tenant` subcommand group : CRUD
// over the tenant registry + tenant-admin / tenant-member management.
//
//	weft tenant ls                                    list every tenant
//	weft tenant create <name> [--domain=<d>]          register a new tenant
//	weft tenant rm <name|uuid>                        delete an empty tenant
//	weft tenant add-admin <tenant> <email>            grant tenant-admin role
//	weft tenant remove-admin <tenant> <email>         revoke tenant-admin role
//	weft tenant add-member <tenant> <email> [groups]  add a member, optionally pinned to project groups
//	weft tenant remove-member <tenant> <email>        remove a member
//
// The webui exposes the same operations under /api/tenants ; this CLI
// surface is the operator-side equivalent so a fresh cluster can be
// brought up through SSH-only access without spinning up the webui.
package tenant

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

// Command returns the `weft tenant` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Manage tenants (top-level multi-tenant boundary)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
		addAdminCmd(socket, sshSocket, sshKey),
		removeAdminCmd(socket, sshSocket, sshKey),
		addMemberCmd(socket, sshSocket, sshKey),
		removeMemberCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List tenants",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListTenants(context.Background(), &weftv1.ListTenantsRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpTenantsJSON(resp.Tenants)
			}
			return renderTenantsTable(resp.Tenants)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var domain string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new tenant (returns its UUID)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateTenant(context.Background(), &weftv1.CreateTenantRequest{
				Name:   args[0],
				Domain: domain,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created\t%s\t%s\t%s\n", resp.Tenant.Uuid, resp.Tenant.Name, resp.Tenant.Domain)
			return nil
		},
	}
	cmd.Flags().StringVar(&domain, "domain", "", "Optional DNS-style tenant domain (e.g. acme.example.com)")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name|uuid>",
		Short: "Delete an empty tenant (refuses if any project still attached)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			uuid, err := ResolveArg(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.DeleteTenant(context.Background(), &weftv1.DeleteTenantRequest{Uuid: uuid}); err != nil {
				return err
			}
			fmt.Println(uuid)
			return nil
		},
	}
}

func addAdminCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "add-admin <name|uuid> <email>",
		Short: "Grant the tenant-admin role to a user (by email)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			tenantUUID, err := ResolveArg(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.AddTenantAdmin(context.Background(), &weftv1.AddTenantAdminRequest{
				TenantUuid: tenantUUID,
				Email:      args[1],
			}); err != nil {
				return err
			}
			fmt.Printf("admin-added\t%s\t%s\n", tenantUUID, args[1])
			return nil
		},
	}
}

func removeAdminCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-admin <name|uuid> <email>",
		Short: "Revoke the tenant-admin role from a user",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			tenantUUID, err := ResolveArg(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.RemoveTenantAdmin(context.Background(), &weftv1.RemoveTenantAdminRequest{
				TenantUuid: tenantUUID,
				Email:      args[1],
			}); err != nil {
				return err
			}
			fmt.Printf("admin-removed\t%s\t%s\n", tenantUUID, args[1])
			return nil
		},
	}
}

func addMemberCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var groups []string
	cmd := &cobra.Command{
		Use:   "add-member <name|uuid> <email>",
		Short: "Add a member to a tenant (--group=<project> repeatable to pin project access)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			tenantUUID, err := ResolveArg(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.AddTenantMember(context.Background(), &weftv1.AddTenantMemberRequest{
				TenantUuid: tenantUUID,
				Email:      args[1],
				Groups:     groups,
			}); err != nil {
				return err
			}
			fmt.Printf("member-added\t%s\t%s\tgroups=%s\n", tenantUUID, args[1], strings.Join(groups, ","))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&groups, "group", nil, "Repeatable : pin the new member to a project (name or UUID)")
	return cmd
}

func removeMemberCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-member <name|uuid> <email>",
		Short: "Remove a member from a tenant",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			tenantUUID, err := ResolveArg(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.RemoveTenantMember(context.Background(), &weftv1.RemoveTenantMemberRequest{
				TenantUuid: tenantUUID,
				Email:      args[1],
			}); err != nil {
				return err
			}
			fmt.Printf("member-removed\t%s\t%s\n", tenantUUID, args[1])
			return nil
		},
	}
}

// ResolveArg returns a UUID from either a literal UUID or a tenant
// display name. Exported so the `quota` subcommand can reuse the
// same resolution rules without a duplicate ListTenants round-trip
// living in two places.
func ResolveArg(c weftv1.WeftAgentClient, arg string) (string, error) {
	if looksLikeUUID(arg) {
		return arg, nil
	}
	resp, err := c.ListTenants(context.Background(), &weftv1.ListTenantsRequest{})
	if err != nil {
		return "", err
	}
	for _, t := range resp.Tenants {
		if t.Name == arg {
			return t.Uuid, nil
		}
	}
	return "", fmt.Errorf("no tenant named %q (use `weft tenant ls` to inspect)", arg)
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

func renderTenantsTable(tenants []*weftv1.TenantInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tNAME\tDOMAIN\tSTATUS\tPROJECTS\tADMINS\tMEMBERS\tCREATED")
	for _, t := range tenants {
		created := time.Unix(0, t.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		domain := t.Domain
		if domain == "" {
			domain = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			t.Uuid, t.Name, domain, t.Status, t.Projects, t.Admins, t.Members, created)
	}
	return tw.Flush()
}

func dumpTenantsJSON(tenants []*weftv1.TenantInfo) error {
	type out struct {
		UUID      string `json:"uuid"`
		Name      string `json:"name"`
		Domain    string `json:"domain,omitempty"`
		Status    string `json:"status"`
		Projects  int32  `json:"projects"`
		Admins    int32  `json:"admins"`
		Members   int32  `json:"members"`
		CreatedAt string `json:"created_at"`
	}
	flat := make([]out, len(tenants))
	for i, t := range tenants {
		flat[i] = out{
			UUID:      t.Uuid,
			Name:      t.Name,
			Domain:    t.Domain,
			Status:    t.Status,
			Projects:  t.Projects,
			Admins:    t.Admins,
			Members:   t.Members,
			CreatedAt: time.Unix(0, t.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
