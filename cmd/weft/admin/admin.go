// Package admin implements `vzc admin …` — operator-side surface
// for control-plane introspection that doesn't fit under the
// per-resource trees (project/user/network/volume/…). Today this
// is the NATS authorization renderer; future siblings will be
// dex-sync / etcd-snapshot / similar control-plane plumbing.
package admin

import (
	"context"
	"fmt"
	"os"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `vzc admin` cobra command + subcommands.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Operator-side control-plane utilities (admin-only)",
	}
	cmd.AddCommand(natsAuthzCommand(socket, sshSocket, sshKey))
	return cmd
}

// natsAuthzCommand pulls the NATS authorization block from vzd's
// project registry and writes it to stdout (or `--out` file). The
// rendered block is the static `authorization { ... }` snippet an
// operator splices into nats.conf — see infra/nats/README.md for
// the reload workflow.
func natsAuthzCommand(socket, sshSocket, sshKey *string) *cobra.Command {
	var adminPubkey, outPath string
	cmd := &cobra.Command{
		Use:   "nats-authz",
		Short: "Render the NATS authorization block from the project registry",
		Long: `Pull the NATS conf "authorization { … }" block from vzd. The
block lists one user entry per project (subscribe allow-list on
that project's wildcard subject, publish denied) plus a
default-deny on everything else.

Admin-gated: the response includes every project's NATS pubkey.

Splice the output into nats.conf and run "nats-server --signal
reload" on each NATS micro-VM to pick up the changes. See
infra/nats/README.md for the full bootstrap.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.RenderNATSAuthorization(context.Background(), &vzdv1.RenderNATSAuthorizationRequest{
				AdminPubkey: adminPubkey,
			})
			if err != nil {
				return fmt.Errorf("RenderNATSAuthorization: %w", err)
			}
			if outPath != "" {
				if err := os.WriteFile(outPath, resp.Config, 0o600); err != nil {
					return fmt.Errorf("write %s: %w", outPath, err)
				}
				fmt.Fprintf(os.Stderr, "vzc admin nats-authz: wrote %d bytes to %s\n", len(resp.Config), outPath)
				return nil
			}
			_, err = os.Stdout.Write(resp.Config)
			return err
		},
	}
	cmd.Flags().StringVar(&adminPubkey, "admin-pubkey", "", "NATS user-NKey pubkey (U…) of the platform itself; granted full pub/sub on vzd.>")
	cmd.Flags().StringVarP(&outPath, "out", "o", "", "Write rendered block to this file (mode 0600) instead of stdout")
	return cmd
}
