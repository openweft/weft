package admin

// vmexport.go implements `weft admin vm-export` — V0.1.9 backup tool
// that dumps the VM registry to HCL with operator-controlled label
// filtering. The canonical use case is excluding `deployment.type=ci`
// records from periodic backups : CI runners are disposable, their
// registry footprint is churn-only, and replaying them on restore
// is at best wasteful and at worst dangerous (running CI jobs that
// the operator never approved).
//
// Output is HCL (the same format the in-etcd per-record blob uses)
// so a future `weft admin vm-import` can replay it without a parser
// rewrite. The label filters compose : --exclude-label is applied
// first, then --include-label keeps only what matches.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// vmExportCommand returns the `weft admin vm-export` cobra command.
func vmExportCommand(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		out         string
		excludeRaw  []string
		includeRaw  []string
		formatJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "vm-export",
		Short: "Dump the VM registry to HCL with label filtering (V0.1.9 backup tool)",
		Long: `Dump the VM registry to HCL with operator-controlled label filtering.

Canonical use case is excluding 'deployment.type=ci' records from
periodic backups : CI runners are disposable and don't need to round-
trip through the snapshot/restore pipeline. Per the convention
documented in docs/operations/etcd-backup.md, run :

  weft admin vm-export --exclude-label deployment.type=ci -o vms.hcl

…inside the etcd snapshot cron. The dump is a list of vm "<uuid>" {}
blocks identical to what etcdctl get vms/ --prefix would print.

--exclude-label is applied first, then --include-label keeps only
what matches. Multiple flag uses AND together (k1=v1 AND k2=v2) ;
within a single value comma-separated alternatives OR (k=a,b).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			excludes, err := parseLabelFilters(excludeRaw)
			if err != nil {
				return fmt.Errorf("--exclude-label: %w", err)
			}
			includes, err := parseLabelFilters(includeRaw)
			if err != nil {
				return fmt.Errorf("--include-label: %w", err)
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListVMs(context.Background(), &weftv1.ListVMsRequest{})
			if err != nil {
				return err
			}
			var filtered []*weftv1.VMInfo
			for _, vm := range resp.Vms {
				properties := vm.Properties // V0.1.9 wire surfaces it via VMInfo
				if matchesAny(properties, excludes) {
					continue
				}
				if len(includes) > 0 && !matchesAll(properties, includes) {
					continue
				}
				filtered = append(filtered, vm)
			}

			w := io.Writer(os.Stdout)
			if out != "" {
				f, err := os.Create(out)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			if formatJSON {
				return shared.PrintJSONTo(w, filtered)
			}
			return writeVMsHCL(w, filtered)
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "Output file path ; default stdout")
	cmd.Flags().StringSliceVar(&excludeRaw, "exclude-label", nil,
		"Drop VMs whose labels match (k=v, repeatable, comma-separated values OR within a flag). Applied first.")
	cmd.Flags().StringSliceVar(&includeRaw, "include-label", nil,
		"Keep only VMs whose labels match every k=v entry. Applied after --exclude-label.")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Emit JSON instead of HCL")
	return cmd
}

// labelClause is one `k=v[,v…]` filter expression. AllowedValues
// nil means "any value, just require the key" — not exposed today
// but kept on the shape for future "key existence" semantics.
type labelClause struct {
	Key           string
	AllowedValues map[string]struct{}
}

func parseLabelFilters(raw []string) ([]labelClause, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]labelClause, 0, len(raw))
	for _, r := range raw {
		k, v, ok := strings.Cut(r, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("bad filter %q (want k=v)", r)
		}
		clause := labelClause{Key: k, AllowedValues: map[string]struct{}{}}
		for _, val := range strings.Split(v, ",") {
			val = strings.TrimSpace(val)
			if val != "" {
				clause.AllowedValues[val] = struct{}{}
			}
		}
		if len(clause.AllowedValues) == 0 {
			return nil, fmt.Errorf("filter %q has empty value", r)
		}
		out = append(out, clause)
	}
	return out, nil
}

// matchesAny returns true if at least one clause matches — used by
// the --exclude path : any match → drop the VM.
func matchesAny(labels map[string]string, clauses []labelClause) bool {
	for _, c := range clauses {
		if _, ok := c.AllowedValues[labels[c.Key]]; ok {
			return true
		}
	}
	return false
}

// matchesAll returns true only when every clause matches — used by
// the --include path : all match → keep the VM.
func matchesAll(labels map[string]string, clauses []labelClause) bool {
	for _, c := range clauses {
		if _, ok := c.AllowedValues[labels[c.Key]]; !ok {
			return false
		}
	}
	return true
}

// writeVMsHCL emits each VM as a single HCL block. The shape mirrors
// the per-record etcd format so a future vm-import can decode this
// dump without a separate parser.
func writeVMsHCL(w io.Writer, vms []*weftv1.VMInfo) error {
	if _, err := fmt.Fprint(w,
		"# weft admin vm-export — V0.1.9 backup dump\n"+
			"# Each block mirrors /weft/vms/<uuid> on the cluster.\n\n",
	); err != nil {
		return err
	}
	for _, vm := range vms {
		if _, err := fmt.Fprintf(w, "vm %q {\n", vm.Uuid); err != nil {
			return err
		}
		fmt.Fprintf(w, "  project_uuid = %q\n", vm.ProjectUuid)
		fmt.Fprintf(w, "  name         = %q\n", vm.Name)
		if vm.Image != "" {
			fmt.Fprintf(w, "  image        = %q\n", vm.Image)
		}
		if vm.Cpu > 0 {
			fmt.Fprintf(w, "  cpu_count    = %d\n", vm.Cpu)
		}
		if vm.MemMb > 0 {
			fmt.Fprintf(w, "  memory_mib   = %d\n", vm.MemMb)
		}
		if len(vm.Properties) > 0 {
			fmt.Fprintln(w, "  properties = {")
			for k, val := range vm.Properties {
				fmt.Fprintf(w, "    %s = %q\n", k, val)
			}
			fmt.Fprintln(w, "  }")
		}
		fmt.Fprintln(w, "}")
		fmt.Fprintln(w)
	}
	return nil
}
