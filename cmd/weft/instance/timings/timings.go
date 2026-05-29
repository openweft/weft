// Package timings implements the `vzc instance timings` sub-
// command — pretty-prints the per-VM lifecycle event log vzd
// records at <vmDir>/timings.jsonl.
//
// Output shape (matches the unified-timeline format that emerged
// from the nano-container-linux boot-time measurement):
//
//	T+   0.00ms (+  0.00ms)  registered                 mode=direct_linux
//	T+   0.60ms (+  0.60ms)  server.start_attempted
//	T+   1.85ms (+  1.25ms)  server.vz_vm_run_forked    pid=39666
//	...
//
// `T+` is relative to the first event in the log; `(+ delta)` is
// the gap from the previous event. Both are in milliseconds with
// 2 decimals — enough precision for any timer the host can see
// without burying the reader in noise.
package timings

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the timings cobra command.
//
// CLI shape:
//
//	vzc instance timings <name> [--format json]
//
// Default format is the human-readable table above. `--format
// json` dumps the raw events array — useful for piping into jq
// or feeding tooling that wants the wall-clock timestamps.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "timings <name>",
		Short: "Show the lifecycle event log for a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.VMTimings(context.Background(), &vzdv1.VMTimingsRequest{Name: args[0]})
			if err != nil {
				return err
			}
			events := resp.Events
			// Defensive: vzd already returns events in wall-clock
			// order, but a future server might break that
			// invariant and this client should still produce a
			// sensible timeline.
			sort.SliceStable(events, func(i, j int) bool {
				return events[i].TsUnixNs < events[j].TsUnixNs
			})
			if format == "json" {
				return dumpJSON(events)
			}
			return renderTimeline(events, args[0])
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// renderTimeline prints the human-readable view. Empty event list
// prints a one-liner so the operator isn't left wondering whether
// the call succeeded.
func renderTimeline(events []*vzdv1.TimingEvent, name string) error {
	if len(events) == 0 {
		fmt.Printf("vm %q: no timings recorded (yet)\n", name)
		return nil
	}
	t0 := events[0].TsUnixNs
	prev := t0
	maxNameLen := 0
	for _, e := range events {
		if len(e.Name) > maxNameLen {
			maxNameLen = len(e.Name)
		}
	}
	fmt.Printf("vm %q — %d event(s), origin = T0 (%s UTC)\n",
		name, len(events), unixNsToString(t0))
	for _, e := range events {
		absMs := float64(e.TsUnixNs-t0) / 1e6
		dMs := float64(e.TsUnixNs-prev) / 1e6
		prev = e.TsUnixNs
		fmt.Printf("  T+%8.2fms (+%6.2fms)  %-*s  %s\n",
			absMs, dMs, maxNameLen, e.Name, formatMeta(e.Meta))
	}
	return nil
}

func dumpJSON(events []*vzdv1.TimingEvent) error {
	type out struct {
		Name      string            `json:"name"`
		TsUnixNs  int64             `json:"ts_unix_ns"`
		Meta      map[string]string `json:"meta,omitempty"`
	}
	flat := make([]out, len(events))
	for i, e := range events {
		flat[i] = out{Name: e.Name, TsUnixNs: e.TsUnixNs, Meta: e.Meta}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}

func formatMeta(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	// Sort keys for stable output — makes diffs across runs more
	// readable for regression-comparison work.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += k + "=" + m[k]
	}
	return out
}

func unixNsToString(ns int64) string {
	// Avoid pulling in time just for this — direct division + a
	// stable RFC3339-ish format keeps the import set tiny.
	const nsPerSec = int64(1e9)
	sec := ns / nsPerSec
	rem := ns % nsPerSec
	return fmt.Sprintf("%d.%09d", sec, rem)
}
