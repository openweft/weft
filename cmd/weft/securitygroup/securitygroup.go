// Package securitygroup implements the `vzc security-group`
// subcommand group: CRUD over vzd's UUID-keyed security-group
// registry, plus an HCL-shaped --rules-file path for atomically
// replacing the rule list.
//
//	vzc security-group ls [--project P] [--format json]
//	vzc security-group create --project P --name N [--description D] [--rules-file rules.hcl]
//	vzc security-group rename <UUID> <new-name>
//	vzc security-group set-description <UUID> <text>
//	vzc security-group set-rules <UUID> --rules-file rules.hcl
//	vzc security-group rm <UUID>
//
// --rules-file accepts an HCL document with one or more `rule { … }`
// blocks. Per [[hcl-over-json]] HCL is the operator-edited format
// of choice; the proto carries the same fields one-to-one.
package securitygroup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/spf13/cobra"
)

// rulesDoc is the HCL schema accepted by --rules-file. One file
// per security-group ruleset, one `rule {}` block per entry.
type rulesDoc struct {
	Rules []ruleBlock `hcl:"rule,block"`
}

type ruleBlock struct {
	Direction       string `hcl:"direction"`
	Protocol        string `hcl:"protocol"`
	PortMin         int    `hcl:"port_min,optional"`
	PortMax         int    `hcl:"port_max,optional"`
	RemoteCIDR      string `hcl:"remote_cidr,optional"`
	RemoteGroupUUID string `hcl:"remote_group_uuid,optional"`
}

// loadRulesFile reads + decodes the HCL ruleset into the wire
// shape vzd expects.
func loadRulesFile(path string) ([]*vzdv1.SecurityRule, error) {
	if path == "" {
		return nil, nil
	}
	var doc rulesDoc
	if err := hclsimple.DecodeFile(path, nil, &doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	out := make([]*vzdv1.SecurityRule, len(doc.Rules))
	for i, r := range doc.Rules {
		out[i] = &vzdv1.SecurityRule{
			Direction:       r.Direction,
			Protocol:        r.Protocol,
			PortMin:         int32(r.PortMin),
			PortMax:         int32(r.PortMax),
			RemoteCidr:      r.RemoteCIDR,
			RemoteGroupUuid: r.RemoteGroupUUID,
		}
	}
	return out, nil
}

func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "security-group",
		Short: "Manage security groups (UUID-keyed, project-scoped)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		renameCmd(socket, sshSocket, sshKey),
		setDescCmd(socket, sshSocket, sshKey),
		setRulesCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List security groups (optionally scoped to one project)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListSecurityGroups(context.Background(), &vzdv1.ListSecurityGroupsRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Groups)
			}
			return renderTable(resp.Groups)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Limit to one project (display name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, name, description, rulesFile string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new security group (rules optional, set via --rules-file)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rules, err := loadRulesFile(rulesFile)
			if err != nil {
				return err
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateSecurityGroup(context.Background(), &vzdv1.CreateSecurityGroupRequest{
				Project:     project,
				Name:        name,
				Description: description,
				Rules:       rules,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created\t%s\t%s\trules=%d\n", resp.Group.Uuid, resp.Group.Name, len(resp.Group.Rules))
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (display name or UUID)")
	cmd.Flags().StringVar(&name, "name", "", "Security-group name (unique within the project)")
	cmd.Flags().StringVar(&description, "description", "", "Free-form description")
	cmd.Flags().StringVar(&rulesFile, "rules-file", "", "Path to an HCL file with `rule { ... }` blocks")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func renameCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <UUID> <new-name>",
		Short: "Rename a security group (UUID unchanged)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.RenameSecurityGroup(context.Background(), &vzdv1.RenameSecurityGroupRequest{
				Uuid: args[0], NewName: args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("renamed\t%s\t%s\n", resp.Group.Uuid, resp.Group.Name)
			return nil
		},
	}
}

func setDescCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set-description <UUID> <text>",
		Short: "Replace the description field",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetSecurityGroupDescription(context.Background(), &vzdv1.SetSecurityGroupDescriptionRequest{
				Uuid: args[0], Description: args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("description\t%s\t%s\n", resp.Group.Uuid, resp.Group.Description)
			return nil
		},
	}
}

func setRulesCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var rulesFile string
	cmd := &cobra.Command{
		Use:   "set-rules <UUID>",
		Short: "Atomically replace the rule list from an HCL file",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			rules, err := loadRulesFile(rulesFile)
			if err != nil {
				return err
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetSecurityGroupRules(context.Background(), &vzdv1.SetSecurityGroupRulesRequest{
				Uuid: args[0], Rules: rules,
			})
			if err != nil {
				return err
			}
			fmt.Printf("rules\t%s\tcount=%d\n", resp.Group.Uuid, len(resp.Group.Rules))
			return nil
		},
	}
	cmd.Flags().StringVar(&rulesFile, "rules-file", "", "Path to an HCL file with `rule { ... }` blocks (required)")
	_ = cmd.MarkFlagRequired("rules-file")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <UUID>",
		Short: "Delete a security group (no cascade onto attached resources)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteSecurityGroup(context.Background(), &vzdv1.DeleteSecurityGroupRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
}

func renderTable(groups []*vzdv1.SecurityGroupInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tPROJECT_UUID\tNAME\tRULES\tDESCRIPTION\tCREATED")
	for _, g := range groups {
		desc := g.Description
		if desc == "" {
			desc = "-"
		}
		created := time.Unix(0, g.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			g.Uuid, g.ProjectUuid, g.Name, len(g.Rules), desc, created)
	}
	return tw.Flush()
}

func dumpJSON(groups []*vzdv1.SecurityGroupInfo) error {
	type ruleOut struct {
		Direction       string `json:"direction"`
		Protocol        string `json:"protocol"`
		PortMin         int32  `json:"port_min,omitempty"`
		PortMax         int32  `json:"port_max,omitempty"`
		RemoteCIDR      string `json:"remote_cidr,omitempty"`
		RemoteGroupUUID string `json:"remote_group_uuid,omitempty"`
	}
	type out struct {
		UUID        string    `json:"uuid"`
		ProjectUUID string    `json:"project_uuid"`
		Name        string    `json:"name"`
		Description string    `json:"description,omitempty"`
		Rules       []ruleOut `json:"rules"`
		CreatedAt   string    `json:"created_at"`
	}
	flat := make([]out, len(groups))
	for i, g := range groups {
		rules := make([]ruleOut, len(g.Rules))
		for j, r := range g.Rules {
			rules[j] = ruleOut{
				Direction:       r.Direction,
				Protocol:        r.Protocol,
				PortMin:         r.PortMin,
				PortMax:         r.PortMax,
				RemoteCIDR:      r.RemoteCidr,
				RemoteGroupUUID: r.RemoteGroupUuid,
			}
		}
		flat[i] = out{
			UUID:        g.Uuid,
			ProjectUUID: g.ProjectUuid,
			Name:        g.Name,
			Description: g.Description,
			Rules:       rules,
			CreatedAt:   time.Unix(0, g.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}

// silence the "imported but not used" warning for strings when only
// the json encoder needs it; keeps the import set deterministic.
var _ = strings.Join
