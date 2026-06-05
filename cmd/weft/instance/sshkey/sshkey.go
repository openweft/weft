// Package sshkey implements `weft instance sshkey` :
// per-VM authorised SSH-key management via WeftAgent's
// {List,Add,Remove}VMSSHKey RPCs.
//
//	weft instance sshkey ls     <vm>                              list keys
//	weft instance sshkey add    <vm> --file <path>                add from file
//	weft instance sshkey add    <vm> --file -                     add from stdin
//	weft instance sshkey add    <vm> --key '<openssh line>'       add inline
//	weft instance sshkey import <vm> --file <authorized_keys>     bulk add (one per line)
//	weft instance sshkey rm     <vm> <fingerprint>                delete by fp
//
// The fingerprint (SHA256:<base64-no-pad>) is the stable identity
// for `rm` — operators don't URL-encode 400 bytes of base64. The
// server parses the OpenSSH public-key line, computes the
// fingerprint, and stores ; re-adding the same key is idempotent
// (which is what makes `import` safe to re-run).
package sshkey

import (
	"bufio"
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

// Command returns the `weft instance sshkey` cobra group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sshkey",
		Short: "Manage per-VM authorised SSH keys",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		addCmd(socket, sshSocket, sshKey),
		importCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project string
		format  string
	)
	cmd := &cobra.Command{
		Use:   "ls <vm>",
		Short: "List a VM's authorised SSH keys",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListVMSSHKeys(context.Background(), &weftv1.ListVMSSHKeysRequest{
				VmName: args[0], Project: project,
			})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Keys)
			}
			return renderTable(resp.Keys)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// addCmd reads the OpenSSH-format public-key line from --file (or
// stdin via --file=-), or accepts it inline via --key. Server parses
// + fingerprints ; idempotent on fingerprint match.
func addCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project string
		file    string
		key     string
	)
	cmd := &cobra.Command{
		Use:   "add <vm>",
		Short: "Add an authorised key (from --file, --key, or stdin via --file=-)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if file == "" && key == "" {
				return errors.New("one of --file or --key is required")
			}
			if file != "" && key != "" {
				return errors.New("--file and --key are mutually exclusive")
			}
			line := key
			if file != "" {
				b, err := readKey(file)
				if err != nil {
					return err
				}
				line = b
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.AddVMSSHKey(context.Background(), &weftv1.AddVMSSHKeyRequest{
				VmName: args[0], Project: project, PublicKey: line,
			})
			if err != nil {
				return err
			}
			fmt.Printf("added\t%s\t%s\t%s\n", args[0], resp.Key.Fingerprint, resp.Key.Comment)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	cmd.Flags().StringVar(&file, "file", "", `Read public-key line from path ("-" = stdin)`)
	cmd.Flags().StringVar(&key, "key", "", "Inline OpenSSH-format public-key line (mutually exclusive with --file)")
	return cmd
}

// importCmd reads an authorized_keys-style file (one OpenSSH public-key
// line per line, blank lines and "#"-prefixed comments skipped) and
// AddVMSSHKey-s each entry. The server's idempotent-on-fingerprint
// contract makes re-import a safe no-op for already-known keys.
func importCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project string
		file    string
	)
	cmd := &cobra.Command{
		Use:   "import <vm>",
		Short: "Bulk-add keys from an authorized_keys-style file (one per line ; idempotent on fingerprint)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			lines, err := readAuthorizedKeys(file)
			if err != nil {
				return err
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			added, skipped := 0, 0
			for _, line := range lines {
				resp, err := c.AddVMSSHKey(context.Background(), &weftv1.AddVMSSHKeyRequest{
					VmName: args[0], Project: project, PublicKey: line,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "skip\t%s\t: %v\n", oneLineSummary(line), err)
					skipped++
					continue
				}
				fmt.Printf("added\t%s\t%s\t%s\n", args[0], resp.Key.Fingerprint, resp.Key.Type)
				added++
			}
			fmt.Printf("imported\t%s\tadded=%d\tskipped=%d\n", args[0], added, skipped)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	cmd.Flags().StringVar(&file, "file", "", `Path to authorized_keys-style file ("-" = stdin) (required)`)
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "rm <vm> <fingerprint>",
		Short: "Remove an authorised key by fingerprint (idempotent)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.RemoveVMSSHKey(context.Background(), &weftv1.RemoveVMSSHKeyRequest{
				VmName: args[0], Project: project, Fingerprint: args[1],
			}); err != nil {
				return err
			}
			fmt.Printf("removed\t%s\t%s\n", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	return cmd
}

// readAuthorizedKeys returns the trimmed non-empty, non-comment lines
// from an authorized_keys-style file. Used by `import`. Accepts "-"
// for stdin to match `add --file -`.
func readAuthorizedKeys(path string) ([]string, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		r = f
	}
	var out []string
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

// oneLineSummary truncates a public-key line for terse error logs ;
// 60 chars is enough to recognise the comment + start of the b64 blob
// without flooding stderr on a multi-line import gone wrong.
func oneLineSummary(line string) string {
	if len(line) > 60 {
		return line[:60] + "…"
	}
	return line
}

// readKey returns the file contents (trimmed of trailing newlines —
// ssh-keygen / gh CLI append one, the server's parseSSHLine doesn't
// care but the trim keeps the wire compact). stdin via "-".
func readKey(path string) (string, error) {
	var raw []byte
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		raw = b
	} else {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		raw = b
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

func renderTable(keys []*weftv1.VMSSHKey) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FINGERPRINT\tTYPE\tCOMMENT\tADDED_AT")
	for _, k := range keys {
		comment := k.Comment
		if comment == "" {
			comment = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.Fingerprint, k.Type, comment, k.AddedAt)
	}
	return tw.Flush()
}

func dumpJSON(keys []*weftv1.VMSSHKey) error {
	type out struct {
		Fingerprint string `json:"fingerprint"`
		Type        string `json:"type"`
		PublicKey   string `json:"public_key"`
		Comment     string `json:"comment"`
		AddedAt     string `json:"added_at"`
	}
	flat := make([]out, len(keys))
	for i, k := range keys {
		flat[i] = out{k.Fingerprint, k.Type, k.PublicKey, k.Comment, k.AddedAt}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
