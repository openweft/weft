// Package federation implements the `weft federation` cobra subtree.
// All verbs operate on the local weft.hcl federation block (join /
// leave / list / place) — there is no federation gRPC RPC : the
// design (docs/design/federation.md §3a) explicitly chose HTTP-pull
// over a control daemon. `list` reads the live poller state from a
// caller-supplied accessor ; `place` reads the same snapshot and
// hands it to federation.Place.
//
// Verbs :
//
//	weft federation join <peer-url> --pubkey <base64-ed25519-pubkey>
//	weft federation leave <peer-url-or-name>
//	weft federation list [--format json]
//	weft federation place --constraints "region=eu-west-3,min_weight=50"
//
// All flag parsing is cobra-only (memory `feedback_cli_cobra`).

package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	pkgfed "github.com/openweft/weft/federation"
	"github.com/spf13/cobra"
)

// Command returns the `weft federation` cobra root. The caller wires
// it into the top-level command in cmd/weft/main.go. Side-effect-free
// at construction time : every verb's RunE owns its own state.
func Command() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "federation",
		Short: "Manage multi-cluster federation peers (lite-mode, HTTP-pull)",
		Long: `Federation-lite primitives : join / leave / list / place. See
docs/design/federation.md for the underlying model. Each cluster
keeps its own etcd ; peers are discovered by polling /cluster-info
on a 30 s cadence (default). place returns a recommendation, not a
binding lease — the operator (or a higher-level controller) still
decides where the workload lands.`,
	}
	cmd.PersistentFlags().StringVar(&configPath, "config", defaultConfigPath(), "Path to weft.hcl (federation block lives here)")
	cmd.AddCommand(
		joinCmd(&configPath),
		leaveCmd(&configPath),
		listCmd(&configPath),
		placeCmd(&configPath),
	)
	return cmd
}

func defaultConfigPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "weft", "weft.hcl")
	}
	return "/etc/weft/weft.hcl"
}

func joinCmd(configPath *string) *cobra.Command {
	var pubkey string
	cmd := &cobra.Command{
		Use:   "join <peer-url>",
		Short: "Add a federation peer to the local weft.hcl (idempotent)",
		Long: `Appends (or refreshes) a peer entry under the federation { } block
of the local weft.hcl. Calling twice with the same URL is a no-op
unless --pubkey changed (key rotation), in which case the new key
replaces the old one. The agent needs a restart to pick up the
change ; v0.3 will plug a live reload.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if pubkey == "" {
				return errors.New("federation join: --pubkey is required (base64-encoded ed25519 public key)")
			}
			return RunJoin(*configPath, args[0], pubkey)
		},
	}
	cmd.Flags().StringVar(&pubkey, "pubkey", "", "Base64-encoded ed25519 public key of the peer (32 bytes raw)")
	return cmd
}

// RunJoin is exported so tests can drive the join flow without
// instantiating a cobra command tree. Idempotent : same args twice
// yield the same on-disk state, byte-for-byte (peers + peer_keys
// are sorted on write).
func RunJoin(configPath, peerURL, pubkey string) error {
	fb, err := pkgfed.ReadFileBlock(configPath)
	if err != nil {
		return err
	}
	if fb == nil {
		fb = &pkgfed.FileBlock{}
	}
	if err := fb.AddPeer(peerURL, pubkey); err != nil {
		return err
	}
	if err := pkgfed.WriteFileBlock(configPath, fb); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "federation: added peer %s (restart weft-agent to apply)\n", peerURL)
	return nil
}

func leaveCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leave <peer-url>",
		Short: "Remove a federation peer from the local weft.hcl",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return RunLeave(*configPath, args[0])
		},
	}
	return cmd
}

// RunLeave drops a peer from the on-disk config. Returns an error
// (not silent success) when the URL isn't a current peer — operator
// who mis-typed gets a "not joined" feedback line.
func RunLeave(configPath, peerURL string) error {
	fb, err := pkgfed.ReadFileBlock(configPath)
	if err != nil {
		return err
	}
	if fb == nil || !fb.RemovePeer(peerURL) {
		return fmt.Errorf("federation: peer %q is not in the local config", peerURL)
	}
	if err := pkgfed.WriteFileBlock(configPath, fb); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "federation: removed peer %s (restart weft-agent to apply)\n", peerURL)
	return nil
}

// SnapshotSource is the seam between the CLI's `list` / `place`
// verbs and a live poller. In production the agent gRPC client
// would surface this through a federation-status RPC ; v0.2 ships
// a CLI-only path that loads the config file and prints the
// configured peers (status = "unconfigured" until the next agent
// run polls them). The seam keeps tests + future live-mode wiring
// trivial — substitute a fixture and assert the table.
type SnapshotSource interface {
	Snapshot() []pkgfed.PeerState
}

// FileSnapshotSource is the v0.2 implementation that reads weft.hcl
// and surfaces the configured peers with Status="unconfigured"
// (LastSeen=zero, LastError=""). The agent's live poller exposes
// the same shape via Poller.Snapshot — when v0.3 adds the RPC, the
// CLI swaps the source without touching the verbs.
type FileSnapshotSource struct {
	Path string
}

// Snapshot implements SnapshotSource.
func (f FileSnapshotSource) Snapshot() []pkgfed.PeerState {
	fb, err := pkgfed.ReadFileBlock(f.Path)
	if err != nil || fb == nil {
		return nil
	}
	out := make([]pkgfed.PeerState, 0, len(fb.Peers))
	for _, u := range fb.Peers {
		out = append(out, pkgfed.PeerState{
			Name:   u,
			URL:    u,
			Status: "unconfigured",
		})
	}
	return out
}

// listSource is the indirection seam tests use to inject fixtures.
// Defaults to FileSnapshotSource ; tests replace it via WithSource.
var listSource = func(path string) SnapshotSource { return FileSnapshotSource{Path: path} }

// WithSource swaps the snapshot source factory. Returns a restore
// func — tests should `defer restore()` to keep package state
// hermetic across runs.
func WithSource(factory func(path string) SnapshotSource) func() {
	old := listSource
	listSource = factory
	return func() { listSource = old }
}

func listCmd(configPath *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Print known federation peers and their status",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return RunList(c.OutOrStdout(), listSource(*configPath).Snapshot(), format)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// RunList renders peers either as JSON or as a tab-aligned table.
// Exported so tests can drive both formats without spinning a
// cobra invocation.
func RunList(out io.Writer, peers []pkgfed.PeerState, format string) error {
	if format == "json" {
		return json.NewEncoder(out).Encode(peers)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tREGION\tLAST-SEEN\tSTATUS")
	for _, p := range peers {
		region := ""
		if p.Manifest != nil {
			for _, m := range p.Manifest.Members {
				if m.Name == p.Name {
					region = m.Region
					break
				}
			}
		}
		lastSeen := "-"
		if !p.LastSeen.IsZero() {
			lastSeen = p.LastSeen.UTC().Format(time.RFC3339)
		}
		status := p.Status
		if status == "" {
			status = "unknown"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, region, lastSeen, status)
	}
	return tw.Flush()
}

func placeCmd(configPath *string) *cobra.Command {
	var constraints string
	var format string
	cmd := &cobra.Command{
		Use:   "place",
		Short: "Recommend the best federation peer for a workload (advisory)",
		Long: `Returns a scored ranking of candidate clusters against the supplied
constraints. The output is a recommendation : the operator (or a
higher-level controller) still picks the target cluster and submits
the workload there directly. See docs/design/federation.md §9 for
the pull-model rationale.

Supported constraint keys (comma-separated key=value pairs) :

  region=eu-west-3   exact-match against Cluster.Region (case-sensitive)
  min_weight=50     filter out candidates below this NormalisedWeight
  exclude=name1,name2  blacklist specific members
`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cons, err := pkgfed.ParseConstraints(constraints)
			if err != nil {
				return err
			}
			peers := listSource(*configPath).Snapshot()
			return RunPlace(c.OutOrStdout(), peers, cons, format)
		},
	}
	cmd.Flags().StringVar(&constraints, "constraints", "", `Constraint expression, e.g. "region=eu-west-3,min_weight=50"`)
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// RunPlace prints the ranking. Empty result means no candidate
// passed the hard filters ; surfaced as a non-zero exit so scripts
// notice. Single best result printed first when text format ; full
// slice printed in JSON.
func RunPlace(out io.Writer, peers []pkgfed.PeerState, c pkgfed.Constraints, format string) error {
	recs := pkgfed.Place(pkgfed.PlaceInput{Peers: peers}, c)
	if format == "json" {
		return json.NewEncoder(out).Encode(recs)
	}
	if len(recs) == 0 {
		fmt.Fprintln(out, "no candidates matched the constraints")
		return errors.New("federation place: no candidates")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCORE\tNAME\tREGION\tWEIGHT\tSTALE\tREASONS")
	for _, r := range recs {
		stale := "false"
		if r.Stale {
			stale = "true"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\n", r.Score, r.Cluster.Name, r.Cluster.Region, r.Cluster.NormalisedWeight(), stale, joinReasons(r.Reasons))
	}
	return tw.Flush()
}

func joinReasons(rs []string) string {
	if len(rs) == 0 {
		return "-"
	}
	out := rs[0]
	for _, r := range rs[1:] {
		out += "; " + r
	}
	return out
}
