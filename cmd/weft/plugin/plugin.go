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
		catalogueCmd(),
	)
	return cmd
}

// ---------------------------------------------------------------
// list — show every plugin in the catalogue + installed instances.
// ---------------------------------------------------------------

func listCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var catalogueDir string
	var stateDir string
	var format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available plugins and installed instances",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			root := resolveCatalogue(catalogueDir)
			cat, err := pluginstore.LoadCatalogue(root)
			if err != nil {
				return err
			}
			store := pluginstore.NewFileStore(resolveStateDir(stateDir))
			installed, err := store.List()
			if err != nil {
				return err
			}
			if format == "json" {
				return printListJSON(cat, installed)
			}
			return renderListTable(cat, installed)
		},
	}
	cmd.Flags().StringVar(&catalogueDir, "catalogue", "", "Catalogue root directory (default: $WEFT_CATALOGUE_DIR or ./catalogue)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "Plugin state directory (default: $XDG_STATE_HOME/weft/plugins)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	// Soak up *socket/sshSocket/sshKey to keep the signature stable
	// — list is offline today, but a future flavour might query the
	// agent for runtime health.
	_, _, _ = socket, sshSocket, sshKey
	return cmd
}

// ---------------------------------------------------------------
// install — apply a manifest, dialling the running agent.
// ---------------------------------------------------------------

func installCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		catalogueDir string
		stateDir     string
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
			root := resolveCatalogue(catalogueDir)
			cat, err := pluginstore.LoadCatalogue(root)
			if err != nil {
				return err
			}
			m, ok := cat[name]
			if !ok {
				return fmt.Errorf("plugin %q not found in catalogue %s", name, root)
			}
			parsed, err := parseInputs(inputs)
			if err != nil {
				return err
			}
			// Resolve early so we error out before dialling
			// the agent if a required input is missing.
			resolved, err := m.ValidateInputs(parsed)
			if err != nil {
				return err
			}
			if dryRun {
				return printDryRun(m, project, resolved)
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			mgr := pluginstore.NewManager(pluginstore.NewAgentClient(c, *socket), pluginstore.NewFileStore(resolveStateDir(stateDir)))
			inst, err := mgr.Install(context.Background(), m, project, asAnyMap(parsed))
			if err != nil {
				return err
			}
			fmt.Printf("installed\t%s\t%s\t%s\n", inst.Name, inst.UUID, project)
			return nil
		},
	}
	cmd.Flags().StringVar(&catalogueDir, "catalogue", "", "Catalogue root directory (default: $WEFT_CATALOGUE_DIR or ./catalogue)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "Plugin state directory (default: $XDG_STATE_HOME/weft/plugins)")
	cmd.Flags().StringVar(&project, "project", "", "Target project (name or UUID)")
	cmd.Flags().StringSliceVar(&inputs, "input", nil, "Input key=value (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate inputs + print the resource plan, don't issue any RPCs")
	return cmd
}

// ---------------------------------------------------------------
// uninstall — tear down a plugin instance.
// ---------------------------------------------------------------

func uninstallCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		stateDir string
		uuid     string
	)
	cmd := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Uninstall a previously-installed plugin instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			store := pluginstore.NewFileStore(resolveStateDir(stateDir))
			if uuid == "" {
				// Auto-resolve when there's exactly one
				// instance ; otherwise we refuse to guess.
				all, err := store.List()
				if err != nil {
					return err
				}
				var matches []pluginstore.Instance
				for _, i := range all {
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
				uuid = matches[0].UUID
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			mgr := pluginstore.NewManager(pluginstore.NewAgentClient(c, *socket), store)
			if err := mgr.Uninstall(context.Background(), name, uuid); err != nil {
				return err
			}
			fmt.Printf("uninstalled\t%s\t%s\n", name, uuid)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "Plugin state directory (default: $XDG_STATE_HOME/weft/plugins)")
	cmd.Flags().StringVar(&uuid, "instance", "", "Instance UUID (auto-resolved when only one instance exists)")
	return cmd
}

// ---------------------------------------------------------------
// status — print the health of installed instances.
// ---------------------------------------------------------------

func statusCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		stateDir string
		format   string
	)
	cmd := &cobra.Command{
		Use:   "status <name>",
		Short: "Show the health of installed instances of a plugin",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store := pluginstore.NewFileStore(resolveStateDir(stateDir))
			all, err := store.List()
			if err != nil {
				return err
			}
			filtered := all
			if len(args) == 1 {
				name := args[0]
				filtered = filtered[:0]
				for _, i := range all {
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
			return renderStatusTable(filtered)
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "Plugin state directory (default: $XDG_STATE_HOME/weft/plugins)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	_, _, _ = socket, sshSocket, sshKey
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
