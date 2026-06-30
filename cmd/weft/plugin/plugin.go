// Package plugin wires the `weft plugin` cobra subtree. The CLI is
// a thin shell over pluginstore.Manager — it loads the catalogue
// (catalogue/<name>/plugin.hcl), dials the running agent, and
// orchestrates Install / Uninstall / List / Status.
//
//	weft plugin list                 # available + installed
//	weft plugin install <name> --input k=v ...
//	weft plugin uninstall <name> [--instance <uuid>]
//	weft plugin status <name>        # health of installed instances
//
// Following the in-tree cobra convention (memory feedback_cli_cobra):
// every flag is declared on a sub-command, not in the package init.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/openweft/weft/pluginstore"
	"github.com/spf13/cobra"
)

// Command builds the `weft plugin` sub-tree.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage weft catalogue plugins (HA topology one-shots)",
		Long: `The plugin catalogue ships ready-made HA topologies (runner
farms, portals) as declarative manifests. One install call provisions
the whole stack — networks, security groups, VMs — through the existing
weft RPCs. Re-running with the same inputs is a no-op (idempotent).`,
	}
	cmd.AddCommand(
		listCmd(socket, sshSocket, sshKey),
		installCmd(socket, sshSocket, sshKey),
		uninstallCmd(socket, sshSocket, sshKey),
		statusCmd(socket, sshSocket, sshKey),
		enableCmd(socket, sshSocket, sshKey),
		disableCmd(socket, sshSocket, sshKey),
		catalogueCmd(),
	)
	return cmd
}

// ---------------------------------------------------------------
// list — show every plugin in the catalogue + installed instances.
// ---------------------------------------------------------------

func listCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available plugins and installed instances",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			catResp, err := c.ListPluginCatalogue(context.Background(), &weftv1.ListPluginCatalogueRequest{})
			if err != nil {
				return err
			}
			instResp, err := c.ListInstalledPlugins(context.Background(), &weftv1.ListInstalledPluginsRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return printListJSONProto(catResp.Entries, instResp.Instances)
			}
			return renderListTableProto(catResp.Entries, instResp.Instances)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// ---------------------------------------------------------------
// install — apply a manifest, dialling the running agent.
// ---------------------------------------------------------------

func installCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		catalogueDir string
		project      string
		inputs       []string
		dryRun       bool
	)
	cmd := &cobra.Command{
		Use:   "install <name>",
		Short: "Install a catalogue plugin into the running cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			parsed, err := parseInputs(inputs)
			if err != nil {
				return err
			}
			if dryRun {
				// Dry-run stays a local-only path : load the
				// catalogue from disk + validate inputs without
				// touching the cluster. Useful when iterating on
				// a plan.hcl before pushing.
				root := resolveCatalogue(catalogueDir)
				cat, err := pluginstore.LoadCatalogue(root)
				if err != nil {
					return err
				}
				m, ok := cat[name]
				if !ok {
					return fmt.Errorf("plugin %q not found in catalogue %s", name, root)
				}
				resolved, err := m.ValidateInputs(parsed)
				if err != nil {
					return err
				}
				return printDryRun(m, project, resolved)
			}
			// V0.4.74 : route install through the agent's InstallPlugin
			// gRPC instead of spinning up a local pluginstore.Manager
			// with its own FileStore. The CLI's old path wrote state
			// to $XDG_STATE_HOME/weft/plugins/ while the agent reads
			// from ~/.weft/plugins/ — a record written by one side
			// was invisible to the other. Single source of truth now.
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.InstallPlugin(context.Background(), &weftv1.InstallPluginRequest{
				Name:    name,
				Project: project,
				Inputs:  stringInputs(parsed),
			})
			if err != nil {
				return err
			}
			fmt.Printf("installed\t%s\t%s\t%s\n", name, resp.InstanceUuid, project)
			return nil
		},
	}
	cmd.Flags().StringVar(&catalogueDir, "catalogue", "", "Catalogue root directory for dry-run (default: $WEFT_CATALOGUE_DIR or ./catalogue)")
	cmd.Flags().StringVar(&project, "project", "", "Target project (name or UUID)")
	cmd.Flags().StringSliceVar(&inputs, "input", nil, "Input key=value (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate inputs + print the resource plan, don't issue any RPCs")
	return cmd
}

// ---------------------------------------------------------------
// uninstall — tear down a plugin instance.
// ---------------------------------------------------------------

func uninstallCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var uuid string
	cmd := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Uninstall a previously-installed plugin instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if uuid == "" {
				// Auto-resolve via the agent's gRPC inventory.
				// Single source of truth (V0.4.74) — was reading
				// from the CLI's own FileStore which diverged from
				// the agent's.
				resp, err := c.ListInstalledPlugins(context.Background(), &weftv1.ListInstalledPluginsRequest{})
				if err != nil {
					return err
				}
				var matches []*weftv1.PluginInstance
				for _, i := range resp.Instances {
					if i.Name == name {
						matches = append(matches, i)
					}
				}
				if len(matches) == 0 {
					return fmt.Errorf("plugin %q has no installed instances", name)
				}
				if len(matches) > 1 {
					return fmt.Errorf("plugin %q has %d installed instances; pass --instance <uuid> to disambiguate", name, len(matches))
				}
				uuid = matches[0].InstanceUuid
			}
			_, err = c.UninstallPlugin(context.Background(), &weftv1.UninstallPluginRequest{
				Name:         name,
				InstanceUuid: uuid,
			})
			if err != nil {
				return err
			}
			fmt.Printf("uninstalled\t%s\t%s\n", name, uuid)
			return nil
		},
	}
	cmd.Flags().StringVar(&uuid, "instance", "", "Instance UUID (auto-resolved when only one instance exists)")
	return cmd
}

// ---------------------------------------------------------------
// status — print the health of installed instances.
// ---------------------------------------------------------------

func statusCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "status <name>",
		Short: "Show the health of installed instances of a plugin",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListInstalledPlugins(context.Background(), &weftv1.ListInstalledPluginsRequest{})
			if err != nil {
				return err
			}
			filtered := resp.Instances
			if len(args) == 1 {
				name := args[0]
				filtered = filtered[:0]
				for _, i := range resp.Instances {
					if i.Name == name {
						filtered = append(filtered, i)
					}
				}
			}
			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(filtered)
			}
			return renderStatusTableProto(filtered)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// ---------------------------------------------------------------
// helpers
// ---------------------------------------------------------------

func parseInputs(kvs []string) (map[string]any, error) {
	out := map[string]any{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --input %q (want key=value)", kv)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// stringInputs converts a parseInputs() map[string]any into the
// map[string]string the InstallPlugin RPC accepts. parseInputs only
// emits string values today ; this is a typed pass-through that
// preserves the (k,v) shape without forcing every caller to switch
// to map[string]string everywhere.
func stringInputs(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func asAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func resolveCatalogue(override string) string {
	if override != "" {
		return override
	}
	return pluginstore.DefaultCatalogueRoot()
}

func resolveStateDir(override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("WEFT_PLUGIN_STATE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "weft", "plugins")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "weft-plugins")
	}
	return filepath.Join(home, ".local", "state", "weft", "plugins")
}

func renderListTable(cat map[string]*pluginstore.Manifest, installed []pluginstore.Instance) error {
	// Count installed instances per plugin.
	count := map[string]int{}
	for _, i := range installed {
		count[i.Name]++
	}
	names := make([]string, 0, len(cat))
	for n := range cat {
		names = append(names, n)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tVERSION\tINSTALLED\tDESCRIPTION")
	for _, n := range names {
		m := cat[n]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", m.Name, m.Kind, m.Version, count[m.Name], m.Description)
	}
	return tw.Flush()
}

func renderStatusTable(items []pluginstore.Instance) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tINSTANCE\tPROJECT\tVMS\tNETWORKS\tSGS\tINSTALLED_AT")
	for _, i := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			i.Name, i.UUID, i.Project, len(i.VMs), len(i.Networks), len(i.SecurityGroups), i.InstalledAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return tw.Flush()
}

// renderListTableProto / renderStatusTableProto / printListJSONProto
// are the V0.4.74 gRPC-fed equivalents of the legacy FileStore
// renderers above. The CLI now reads catalogue + installed state
// from the agent so output matches what `ListInstalledPlugins`
// returns rather than diverging via a CLI-only on-disk store.
func renderListTableProto(cat []*weftv1.PluginCatalogueEntry, installed []*weftv1.PluginInstance) error {
	count := map[string]int{}
	for _, i := range installed {
		count[i.Name]++
	}
	sort.Slice(cat, func(i, j int) bool { return cat[i].Name < cat[j].Name })
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tVERSION\tINSTALLED\tDESCRIPTION")
	for _, m := range cat {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", m.Name, m.Kind, m.Version, count[m.Name], m.Description)
	}
	return tw.Flush()
}

func renderStatusTableProto(items []*weftv1.PluginInstance) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tINSTANCE\tPROJECT\tVMS\tSTATUS\tINSTALLED_AT")
	for _, i := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			i.Name, i.InstanceUuid, i.Project, len(i.VmUuids), i.Status,
			time.Unix(0, i.InstalledAtUnixNs).UTC().Format("2006-01-02T15:04:05Z"))
	}
	return tw.Flush()
}

func printListJSONProto(cat []*weftv1.PluginCatalogueEntry, installed []*weftv1.PluginInstance) error {
	type entry struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Version     string `json:"version"`
		Description string `json:"description"`
		Installed   int    `json:"installed"`
	}
	count := map[string]int{}
	for _, i := range installed {
		count[i.Name]++
	}
	out := make([]entry, 0, len(cat))
	for _, m := range cat {
		out = append(out, entry{Name: m.Name, Kind: m.Kind, Version: m.Version, Description: m.Description, Installed: count[m.Name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printListJSON(cat map[string]*pluginstore.Manifest, installed []pluginstore.Instance) error {
	type entry struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Version     string `json:"version"`
		Description string `json:"description"`
		Installed   int    `json:"installed"`
	}
	count := map[string]int{}
	for _, i := range installed {
		count[i.Name]++
	}
	out := make([]entry, 0, len(cat))
	for _, m := range cat {
		out = append(out, entry{Name: m.Name, Kind: m.Kind, Version: m.Version, Description: m.Description, Installed: count[m.Name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printDryRun(m *pluginstore.Manifest, project string, resolved map[string]string) error {
	fmt.Printf("dry-run\t%s\tproject=%s\n", m.Name, project)
	fmt.Printf("  networks: %d\n", len(m.Networks))
	fmt.Printf("  security_groups: %d\n", len(m.SecurityGroups))
	total := 0
	for _, vm := range m.VMs {
		fmt.Printf("  vm %q: replicas=%d image=%s\n", vm.Name, vm.Replicas, vm.Image)
		total += vm.Replicas
	}
	fmt.Printf("  total VMs: %d\n", total)
	// Inputs (with secrets masked).
	keys := make([]string, 0, len(resolved))
	for k := range resolved {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	secretSet := map[string]bool{}
	for _, in := range m.Inputs {
		if in.Secret {
			secretSet[in.Name] = true
		}
	}
	for _, k := range keys {
		v := resolved[k]
		if secretSet[k] {
			v = "***"
		}
		fmt.Printf("  input %s=%s\n", k, v)
	}
	return nil
}
