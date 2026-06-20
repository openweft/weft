// Package project implements the `weft project` subcommand group:
// CRUD over weft's project registry.
//
//	weft project ls                          list every project
//	weft project create <name>               register a new project
//	weft project rename <name|uuid> <new>    rename by UUID or current name
//	weft project rm <name|uuid>              delete (only when empty)
//
// The whole point of the UUID-keyed registry is that `rename` is a
// metadata-only change — every VM attached to the project keeps
// resolving by either its old UUID OR its new display name.
package project

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

// Command returns the `weft project` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage projects (UUID-keyed namespaces for VMs)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		renameCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
		setTenantCmd(socket, sshSocket, sshKey),
		addUserCmd(socket, sshSocket, sshKey),
		removeUserCmd(socket, sshSocket, sshKey),
		membersCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List projects",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListProjects(context.Background(), &weftv1.ListProjectsRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpProjectsJSON(resp.Projects)
			}
			return renderProjectsTable(resp.Projects)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new project (returns its UUID)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateProject(context.Background(), &weftv1.CreateProjectRequest{Name: args[0]})
			if err != nil {
				return err
			}
			tag := "created"
			if !resp.Created {
				tag = "exists"
			}
			fmt.Printf("%s\t%s\t%s\n", tag, resp.Project.Uuid, resp.Project.Name)
			return nil
		},
	}
}

// setTenantCmd binds (or unbinds) a project to a parent tenant.
// Empty second arg unbinds the project ; pass `--clear` for the
// same effect with a clearer signal in the shell history. Powers
// the GetProjectQuota.siblings_total + tenant_cap aggregation
// (weft v0.4.37/0.4.38 ; weft-proto v0.13.0 SetProjectTenant RPC).
func setTenantCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "set-tenant <project-name|uuid> [tenant-uuid]",
		Short: "Bind a project to a parent tenant (empty tenant or --clear to unbind)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			projUUID, err := resolveProjectArg(c, args[0])
			if err != nil {
				return err
			}
			var tenantUUID string
			if !clear && len(args) == 2 {
				tenantUUID = args[1]
			}
			resp, err := c.SetProjectTenant(context.Background(), &weftv1.SetProjectTenantRequest{
				ProjectUuid: projUUID,
				TenantUuid:  tenantUUID,
			})
			if err != nil {
				return err
			}
			if resp.Project.TenantUuid == "" {
				fmt.Printf("unbound\t%s\t%s\n", resp.Project.Uuid, resp.Project.Name)
			} else {
				fmt.Printf("bound\t%s\t%s\ttenant=%s\n", resp.Project.Uuid, resp.Project.Name, resp.Project.TenantUuid)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "Unbind from any tenant (equivalent to passing an empty tenant-uuid)")
	return cmd
}

// renameCmd takes either a UUID or the current display name as the
// first argument. When a name is passed we resolve it to a UUID
// client-side via a ListProjects call — keeps weft's RenameProject
// RPC strict (UUID-only) so renames can't silently target a
// different project than the operator meant.
func renameCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <name|uuid> <new-name>",
		Short: "Rename a project (UUID stays, every attached VM follows automatically)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			uuid, err := resolveProjectArg(c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.RenameProject(context.Background(), &weftv1.RenameProjectRequest{
				Uuid:    uuid,
				NewName: args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("renamed\t%s\t%s\n", resp.Project.Uuid, resp.Project.Name)
			return nil
		},
	}
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name|uuid>",
		Short: "Delete an empty project",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			uuid, err := resolveProjectArg(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.DeleteProject(context.Background(), &weftv1.DeleteProjectRequest{Uuid: uuid}); err != nil {
				return err
			}
			fmt.Println(uuid)
			return nil
		},
	}
}

// resolveProjectArg returns a UUID from either a literal UUID or a
// display name. A name that doesn't match any project errors out
// rather than silently auto-creating — `rename`/`rm` should never
// invent a project.
func resolveProjectArg(c weftv1.WeftAgentClient, arg string) (string, error) {
	if looksLikeUUID(arg) {
		return arg, nil
	}
	resp, err := c.ListProjects(context.Background(), &weftv1.ListProjectsRequest{})
	if err != nil {
		return "", err
	}
	for _, p := range resp.Projects {
		if p.Name == arg {
			return p.Uuid, nil
		}
	}
	return "", fmt.Errorf("no project named %q (use `weft project ls` to inspect)", arg)
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

func renderProjectsTable(projects []*weftv1.ProjectInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tNAME\tCREATED")
	for _, p := range projects {
		created := time.Unix(0, p.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Uuid, p.Name, created)
	}
	return tw.Flush()
}

// addUserCmd grants project access by user-UUID (the
// platform-managed path that doesn't go through dex). The first
// arg resolves to a project UUID via resolveProjectArg (name or
// UUID accepted); the second arg is a literal user UUID from
// `weft user ls`.
func addUserCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "add-user <name|uuid> <user-uuid>",
		Short: "Grant project access to a user (admin only)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			projectUUID, err := resolveProjectArg(c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.AddProjectMember(context.Background(), &weftv1.AddProjectMemberRequest{
				ProjectUuid: projectUUID,
				UserUuid:    args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("added\t%s\t%s\tmembers=%d\n", projectUUID, args[1], len(resp.UserUuids))
			return nil
		},
	}
}

// removeUserCmd revokes the platform-side grant. (A `project:<uuid>`
// dex group claim, if also present, still grants access — that's
// a separate revocation on the IdP side.)
func removeUserCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-user <name|uuid> <user-uuid>",
		Short: "Revoke a platform-managed project membership (admin only)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			projectUUID, err := resolveProjectArg(c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.RemoveProjectMember(context.Background(), &weftv1.RemoveProjectMemberRequest{
				ProjectUuid: projectUUID,
				UserUuid:    args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("removed\t%s\t%s\tmembers=%d\n", projectUUID, args[1], len(resp.UserUuids))
			return nil
		},
	}
}

// membersCmd lists the project's platform-managed members. The
// server side AuthorizeProject gates this so non-members can't
// probe membership.
//
// --resolve does one GetUser round-trip per member to surface
// display name + email alongside the UUID. Helpful for "who's in
// this project?" audits; pricier than the bare list (N+1 RPCs)
// so it's opt-in.
func membersCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var resolve bool
	cmd := &cobra.Command{
		Use:   "members <name|uuid>",
		Short: "List the project's platform-managed member UUIDs",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			projectUUID, err := resolveProjectArg(c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.ListProjectMembers(context.Background(), &weftv1.ListProjectMembersRequest{
				ProjectUuid: projectUUID,
			})
			if err != nil {
				return err
			}
			if len(resp.UserUuids) == 0 {
				fmt.Println("(no platform-managed members)")
				return nil
			}
			if !resolve {
				for _, u := range resp.UserUuids {
					fmt.Println(u)
				}
				return nil
			}
			// Resolve mode: tab-separated UUID / DISPLAY_NAME /
			// EMAIL. A failed GetUser (user since deleted, not
			// visible to caller) renders as "-" rather than aborting
			// the whole listing.
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UUID\tDISPLAY_NAME\tEMAIL")
			for _, u := range resp.UserUuids {
				name, email := "-", "-"
				if r, err := c.GetUser(context.Background(), &weftv1.GetUserRequest{Uuid: u}); err == nil {
					if r.User.DisplayName != "" {
						name = r.User.DisplayName
					}
					if r.User.Email != "" {
						email = r.User.Email
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", u, name, email)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&resolve, "resolve", false, "Resolve each UUID to its display name + email (N+1 RPCs)")
	return cmd
}

func dumpProjectsJSON(projects []*weftv1.ProjectInfo) error {
	type out struct {
		UUID       string `json:"uuid"`
		Name       string `json:"name"`
		CreatedAt  string `json:"created_at"`
		TenantUUID string `json:"tenant_uuid,omitempty"`
	}
	flat := make([]out, len(projects))
	for i, p := range projects {
		flat[i] = out{
			UUID:       p.Uuid,
			Name:       p.Name,
			CreatedAt:  time.Unix(0, p.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
			TenantUUID: p.TenantUuid,
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
