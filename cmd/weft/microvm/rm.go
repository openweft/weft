// rm.go implements `weft microvm rm` — the Docker-`rm` analogue for
// microVMs.
//
// Each argument resolves to an agent VM name, accepting either the
// bare image ref (`alpine:3.21` — same normalisation `run` applies)
// or the already-prefixed VM name (`ncl-alpine_3.21`). Each VM is
// best-effort stopped via StopVM, then deleted via DeleteVM. A stop
// error on an already-stopped VM is tolerated so `rm` is idempotent —
// the only authoritative step is DeleteVM. `-f` skips the stop call
// entirely (matches `docker rm -f`).
package microvm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// rmCmd returns the `weft microvm rm` command.
func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		force   bool
		project string
	)
	cmd := &cobra.Command{
		Use:   "rm NAME...",
		Short: "Stop and delete microVMs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			var firstErr error
			for _, raw := range args {
				vmName := resolveName(raw)
				if err := removeOne(c, project, vmName, force); err != nil {
					fmt.Fprintf(os.Stderr, "weft microvm rm: %s: %v\n", vmName, err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				fmt.Fprintln(os.Stdout, vmName)
			}
			return firstErr
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "skip the stop step (force-delete)")
	cmd.Flags().StringVar(&project, "project", "", "project namespace")
	return cmd
}

// resolveName maps either a bare image reference (`alpine:3.21`) or an
// already-prefixed VM name (`ncl-alpine_3.21`) into the form the agent
// stored. The image-ref → name transform mirrors the run path so
// `weft microvm rm alpine:3.21` cleans up exactly what
// `weft microvm run alpine:3.21` created.
func resolveName(raw string) string {
	if strings.HasPrefix(raw, vmNamePrefix) {
		return raw
	}
	r := strings.NewReplacer("/", "_", ":", "_")
	return vmNamePrefix + r.Replace(raw)
}

// removeOne stops + deletes a single VM. Stop is best-effort (an
// already-stopped VM is fine); Delete is authoritative.
func removeOne(c vzdv1.WeftAgentClient, project, vmName string, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !force {
		if _, err := c.StopVM(ctx, &vzdv1.StopVMRequest{Name: vmName, Project: project}); err != nil {
			if !isAlreadyStopped(err) {
				return fmt.Errorf("stop: %w", err)
			}
		}
	}
	if _, err := c.DeleteVM(ctx, &vzdv1.DeleteVMRequest{Name: vmName, Project: project}); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// isAlreadyStopped detects the agent's "not running" error so `rm`
// stays idempotent. Matches on substring rather than a gRPC code
// because the agent currently returns a plain error for this case.
func isAlreadyStopped(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "not running") ||
		strings.Contains(s, "already stopped") ||
		strings.Contains(s, "STOPPED")
}
