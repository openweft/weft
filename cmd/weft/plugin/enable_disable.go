package plugin

// enable_disable.go — operator-facing CLI mirrors of the EnablePlugin /
// DisablePlugin RPCs. The two commands use the agent RPC (not a local
// pluginstore.Manager.SetDisabled) because the StateStore lives on
// the agent — running these locally against a fresh ~/.local/share
// directory wouldn't flip the running cluster's view.
//
// UX :
//
//   weft plugin enable  <name> [--instance <uuid>]
//   weft plugin disable <name> [--instance <uuid>]
//
// --instance is auto-resolved when exactly one installed instance
// of <name> exists (mirrors uninstallCmd's ergonomics). Both
// commands are idempotent on the agent — flipping a flag that's
// already in the requested state returns success.

import (
	"context"
	"fmt"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/spf13/cobra"
)

func enableCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return setDisabledCmd(socket, sshSocket, sshKey, false, "enable", "Re-activate a disabled plugin instance")
}

func disableCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return setDisabledCmd(socket, sshSocket, sshKey, true, "disable", "Mark a plugin instance inactive without uninstalling it")
}

// setDisabledCmd is the shared body for enable / disable so the two
// commands can't drift in flag layout, UUID resolution, or output
// formatting.
func setDisabledCmd(socket, sshSocket, sshKey *string, disabled bool, verb, short string) *cobra.Command {
	var uuid string
	cmd := &cobra.Command{
		Use:   verb + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if uuid == "" {
				resolved, err := resolveSingleInstanceUUID(c, name)
				if err != nil {
					return err
				}
				uuid = resolved
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if disabled {
				_, err = c.DisablePlugin(ctx, &weftv1.DisablePluginRequest{Name: name, InstanceUuid: uuid})
			} else {
				_, err = c.EnablePlugin(ctx, &weftv1.EnablePluginRequest{Name: name, InstanceUuid: uuid})
			}
			if err != nil {
				return err
			}
			fmt.Printf("%sd\t%s\t%s\n", verb, name, uuid)
			return nil
		},
	}
	cmd.Flags().StringVar(&uuid, "instance", "", "Instance UUID (auto-resolved when only one instance exists)")
	return cmd
}

// resolveSingleInstanceUUID queries the agent's ListInstalledPlugins
// and returns the instance UUID iff exactly one instance of `name`
// is installed. Auto-resolution keeps the common case (one instance
// per plugin) ergonomic without exposing the UUID guess to the
// multi-instance path — that gets a clear error instead.
func resolveSingleInstanceUUID(c weftv1.WeftAgentClient, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.ListInstalledPlugins(ctx, &weftv1.ListInstalledPluginsRequest{})
	if err != nil {
		return "", fmt.Errorf("list installed: %w", err)
	}
	var matches []string
	for _, p := range resp.Instances {
		if p.Name == name {
			matches = append(matches, p.InstanceUuid)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("plugin %q has no installed instances", name)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("plugin %q has %d installed instances ; pass --instance <uuid> to disambiguate", name, len(matches))
	}
}
