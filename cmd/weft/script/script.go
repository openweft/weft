// Package script implements the `weft script` subcommand group :
// CRUD over the cluster-wide provisioning-script catalogue exposed
// by weft's WeftAgent.{List,Get,Set,Delete}Script RPCs.
//
//	weft script ls                          list every script
//	weft script get <name>                  print one script (body included)
//	weft script set <name> --file <path>    create or update from a file (admin)
//	weft script set <name> --body <sh>      set body inline (admin)
//	weft script rm  <name>                  delete (admin)
//
// Body is read from --file (or stdin if --file=-) so an operator
// can pipe the editor's output without wrestling shell quoting :
//
//	weft script set deploy-nginx --file deploy-nginx.sh --description "…"
//	cat my.sh | weft script set deploy --file - --description "ad-hoc"
//
// Body is the literal sh source ; the in-guest weft-microvm-agent picks
// it up by name from the VM's weft.boot/script property and runs
// it via mvdan.cc/sh/v3.
package script

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft script` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "script",
		Short: "Manage the cluster-wide provisioning-script catalogue",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		getCmd(socket, sshSocket, sshKey),
		setCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List scripts (name + metadata, no body)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListScripts(context.Background(), &weftv1.ListScriptsRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Scripts)
			}
			return renderTable(resp.Scripts)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// getCmd prints one script. --body-only writes just the sh source
// to stdout — handy for piping into the operator's editor:
//
//	weft script get deploy --body-only > deploy.sh
func getCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		format   string
		bodyOnly bool
	)
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show one script (body included)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetScript(context.Background(), &weftv1.GetScriptRequest{Name: args[0]})
			if err != nil {
				return err
			}
			if bodyOnly {
				_, err := io.WriteString(os.Stdout, resp.Script.Body)
				return err
			}
			if format == "json" {
				return dumpJSON([]*weftv1.Script{resp.Script})
			}
			return renderOne(resp.Script)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	cmd.Flags().BoolVar(&bodyOnly, "body-only", false, "Write only the sh body to stdout (for piping into an editor)")
	return cmd
}

// setCmd reads the body from --file (or stdin via --file=-) and
// uploads. --description is optional. UpdatedAt + UpdatedBy are
// server-stamped — the wire side can't lie about provenance.
func setCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		file        string
		body        string
		description string
	)
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a script (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if file == "" && body == "" {
				return errors.New("one of --file or --body is required")
			}
			if file != "" && body != "" {
				return errors.New("--file and --body are mutually exclusive")
			}
			payload := body
			if file != "" {
				b, err := readBody(file)
				if err != nil {
					return err
				}
				payload = b
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetScript(context.Background(), &weftv1.SetScriptRequest{
				Script: &weftv1.Script{
					Name:        args[0],
					Description: description,
					Body:        payload,
				},
			})
			if err != nil {
				return err
			}
			fmt.Printf("set\t%s\tlines=%d\tupdated_by=%s\n",
				resp.Script.Name,
				1+strings.Count(resp.Script.Body, "\n"),
				resp.Script.UpdatedBy)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", `Read body from path ("-" = stdin)`)
	cmd.Flags().StringVar(&body, "body", "", "Set body inline (mutually exclusive with --file)")
	cmd.Flags().StringVar(&description, "description", "", "Short description shown in `ls`")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a script (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.DeleteScript(context.Background(), &weftv1.DeleteScriptRequest{Name: args[0]})
			if err != nil {
				return err
			}
			fmt.Printf("deleted\t%s\n", resp.Deleted)
			return nil
		},
	}
}

// readBody returns the file contents or stdin when path = "-".
// stdin support means the operator can `cat my.sh | weft script
// set X --file -` without a temp file dance.
func readBody(path string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), nil
}

func renderTable(scripts []*weftv1.Script) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLINES\tUPDATED_AT\tUPDATED_BY\tDESCRIPTION")
	for _, s := range scripts {
		lines := 1 + strings.Count(s.Body, "\n")
		desc := s.Description
		if desc == "" {
			desc = "-"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", s.Name, lines, s.UpdatedAt, s.UpdatedBy, desc)
	}
	return tw.Flush()
}

// renderOne prints the header + body of a single script (the "cat
// my.sh" style ; body comes last so the meta header doesn't pollute
// a `weft script get X | sh` pipeline).
func renderOne(s *weftv1.Script) error {
	fmt.Printf("# name        : %s\n", s.Name)
	fmt.Printf("# description : %s\n", s.Description)
	fmt.Printf("# updated_at  : %s\n", s.UpdatedAt)
	fmt.Printf("# updated_by  : %s\n", s.UpdatedBy)
	fmt.Println("# ---")
	_, err := io.WriteString(os.Stdout, s.Body)
	return err
}

func dumpJSON(scripts []*weftv1.Script) error {
	type out struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
		UpdatedAt   string `json:"updated_at"`
		UpdatedBy   string `json:"updated_by"`
	}
	flat := make([]out, len(scripts))
	for i, s := range scripts {
		flat[i] = out{s.Name, s.Description, s.Body, s.UpdatedAt, s.UpdatedBy}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
