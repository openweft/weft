// Package events implements `vzc events` — opens a WatchEvents
// stream against vzd and prints every event in human or JSON
// form. Closes cleanly on Ctrl-C; the cached OIDC token (from
// `vzc login`) reaches vzd via the bearer interceptor in
// vzclient.Dial.
package events

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/openweft/weft-client"
	"github.com/spf13/cobra"
)

// Command returns the `vzc events` cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var kindPrefixes []string
	var project string
	var vm string
	var format string
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream vzd platform events (VMs, projects, lifecycle) live",
		Long: `Open a server-streaming WatchEvents subscription against vzd
and print every event as it arrives. The bus is ACL-scoped
server-side, so non-admin callers only see events from the
projects their OIDC token grants access to.

Filter by kind with --kind-prefix vm. (every VM event) or
--kind-prefix project. (every project mutation). Filter to one
project with --project NAME-OR-UUID.

Stop with Ctrl-C.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			return vzclient.StreamEvents(ctx, c, vzclient.EventStreamOptions{
				KindPrefixes: kindPrefixes,
				Project:      project,
				Subject:      vm,
				Format:       format,
			}, os.Stdout)
		},
	}
	cmd.Flags().StringSliceVar(&kindPrefixes, "kind-prefix", nil, "Filter events by kind prefix (repeatable): vm. / project. / guest. / ...")
	cmd.Flags().StringVar(&project, "project", "", "Limit events to a specific project (display name or UUID)")
	cmd.Flags().StringVar(&vm, "vm", "", "Limit events to one VM (matches event.subject exactly; canonical 'what is this VM doing now' filter)")
	cmd.Flags().StringVar(&format, "format", "", "Output format: empty (human, tab-separated) or json (one object per line)")
	return cmd
}
