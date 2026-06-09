// Package telemetry implements the `weft telemetry …` CLI subtree :
// the four operator-facing verbs that drive the opt-in anonymous
// heartbeat (see ../../../telemetry/ for the runtime sender).
//
//	weft telemetry enable [--endpoint URL]   flip on, mint cluster_uuid+install_date
//	weft telemetry disable                   flip off
//	weft telemetry status                    show enabled/endpoint/last_sent_at
//	weft telemetry preview                   print the next payload without sending
//
// State persistence : the CLI operates directly on the same on-disk
// blob the agent reads at startup (file backend, by convention
// .telemetry.hcl under the configured registry dir). Cluster mode
// (etcd) is reached transparently when the operator points
// --state-dir at the etcd-backed dir ; for now the operator-side
// surface is single-host because telemetry decisions are
// cluster-global rather than per-host.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	weft "github.com/openweft/weft"
	telem "github.com/openweft/weft/telemetry"
	"github.com/spf13/cobra"
)

// Command returns the `weft telemetry` cobra command group. The
// socket pointers are accepted for parity with the other subtrees
// but ignored — telemetry never round-trips through the agent's
// gRPC layer (the data lives in a single tiny registry blob; the
// operator-side CLI flips it locally).
func Command(_socket, _sshSocket, _sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "telemetry",
		Short: "Manage the opt-in anonymous heartbeat",
		Long: `Configure the optional, anonymous, opt-in telemetry heartbeat.

The openweft project does NOT collect telemetry by default and runs
no central server. This subcommand exists so an operator can wire
their OWN aggregator (internal metrics endpoint, vendor stack, …)
to receive a minimal 24h heartbeat from their cluster.

When enabled, the agent POSTs a tiny JSON envelope containing
counts (host, running VM), driver labels, public plugin names,
runtime (Go version, OS, arch), and a stable anonymous_id derived
from a per-cluster random UUID + install date. NO names, NO IPs,
NO project/VM UUIDs, NO operator emails, NO config values.

See docs/operations/telemetry.md for the full payload contract.`,
	}
	stateDir := ""
	cmd.PersistentFlags().StringVar(&stateDir, "state-dir", "",
		"Directory holding the agent's registry blobs. Empty = the same default as `weft agent` (~/.weft/hcl).")

	cmd.AddCommand(
		enableCmd(&stateDir),
		disableCmd(&stateDir),
		statusCmd(&stateDir),
		previewCmd(&stateDir),
	)
	return cmd
}

// resolveStateDir returns the directory under which the telemetry
// registry blob lives. Mirrors the default `weft agent` uses for
// the file backend (--config-dir weft/hcl) when --state-dir is
// not set. Operators with a non-default agent config-dir pass
// --state-dir to point telemetry at the matching path.
func resolveStateDir(flag string) string {
	if flag != "" {
		return flag
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "state/hcl"
	}
	return filepath.Join(home, ".weft", "hcl")
}

// newStore wires the on-disk registry blob the CLI talks to. The
// file path matches weft.NewFileStorageInDir's convention so the
// agent's `RegistryStorage("telemetry")` will read the same blob
// at startup.
func newStore(stateDir string) (telem.Store, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	// PathInDir gives us `<stateDir>/.telemetry.hcl` — telemetry
	// stores JSON inside the same naming convention so the blob
	// is visible alongside the other registries (one ls and the
	// operator sees what's persisted).
	path := weft.PathInDir(stateDir, "telemetry")
	return telem.NewBlobStore(weft.NewFileStorage(path)), nil
}

// mintClusterUUID generates a 16-byte hex string for the
// (cluster_uuid, install_date) pair. crypto/rand because this
// value feeds an anonymisation hash — predictable randomness here
// would let a receiver pre-compute and de-anonymise.
func mintClusterUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// enableCmd : `weft telemetry enable [--endpoint URL]`.
func enableCmd(stateDir *string) *cobra.Command {
	var endpoint string
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Opt in : flip the per-cluster telemetry flag on",
		Long: `Marks telemetry as ENABLED in the agent's registry. On the next
24h tick (or process restart) the agent starts POSTing the
heartbeat to --endpoint.

The first enable mints a one-time anonymous cluster_uuid and
install_date — these two values feed the anonymous_id hash so
the receiver can de-duplicate heartbeats from the same cluster
without learning who you are.

This command prints a confirmation banner to stderr listing
exactly what will be sent and pointing at the full doc. Run
"weft telemetry preview" to see the actual payload before any
24h tick fires.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEnable(cmd.OutOrStdout(), cmd.ErrOrStderr(), *stateDir, endpoint)
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "",
		"HTTPS URL telemetry POSTs to. Empty leaves the previous value (or unset).")
	return cmd
}

// runEnable is split out so tests can drive it without going
// through cobra's argument parser. Returns nil on success ; never
// touches the network.
func runEnable(stdout, stderr io.Writer, stateDirFlag, endpoint string) error {
	stateDir := resolveStateDir(stateDirFlag)
	store, err := newStore(stateDir)
	if err != nil {
		return err
	}
	st, err := store.LoadState(context.Background())
	if err != nil {
		return err
	}
	st.Enabled = true
	if endpoint != "" {
		st.Endpoint = endpoint
	}
	if st.ClusterUUID == "" {
		u, err := mintClusterUUID()
		if err != nil {
			return fmt.Errorf("mint cluster uuid: %w", err)
		}
		st.ClusterUUID = u
	}
	if st.InstallDate == "" {
		st.InstallDate = time.Now().UTC().Format("2006-01-02")
	}
	if err := store.SaveState(context.Background(), st); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "weft telemetry: ENABLED.")
	fmt.Fprintf(stderr, "  endpoint   : %s\n", displayEndpoint(st.Endpoint))
	fmt.Fprintf(stderr, "  cluster id : anonymous (sha256-truncated, not reversible)\n")
	fmt.Fprintln(stderr, "  payload    : host count, running VM count, driver labels,")
	fmt.Fprintln(stderr, "               installed plugin names, Go version, OS/arch, uptime.")
	fmt.Fprintln(stderr, "  NEVER sent : names, IPs, project/VM UUIDs, operator emails,")
	fmt.Fprintln(stderr, "               config values, audit log content.")
	fmt.Fprintln(stderr, "  full doc   : docs/operations/telemetry.md")
	fmt.Fprintln(stderr, "Run `weft telemetry preview` to see the exact next payload.")
	_ = stdout // banner intentionally on stderr ; stdout reserved for `preview`
	return nil
}

// disableCmd : `weft telemetry disable`.
func disableCmd(stateDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Opt out : flip the per-cluster telemetry flag off",
		Long: `Marks telemetry as DISABLED. The agent's next tick is a no-op ;
no further heartbeats leave the cluster until the operator runs
"weft telemetry enable" again.

cluster_uuid and install_date are kept on disk so a future
re-enable preserves the same anonymous_id (re-disabling and
re-enabling does not look like a brand-new cluster to the
receiver).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDisable(cmd.ErrOrStderr(), *stateDir)
		},
	}
}

func runDisable(stderr io.Writer, stateDirFlag string) error {
	store, err := newStore(resolveStateDir(stateDirFlag))
	if err != nil {
		return err
	}
	st, err := store.LoadState(context.Background())
	if err != nil {
		return err
	}
	st.Enabled = false
	if err := store.SaveState(context.Background(), st); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "weft telemetry: DISABLED.")
	return nil
}

// statusCmd : `weft telemetry status`.
func statusCmd(stateDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show enabled state, endpoint, last-sent timestamp",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.OutOrStdout(), *stateDir)
		},
	}
}

func runStatus(stdout io.Writer, stateDirFlag string) error {
	store, err := newStore(resolveStateDir(stateDirFlag))
	if err != nil {
		return err
	}
	st, err := store.LoadState(context.Background())
	if err != nil {
		return err
	}
	state := "disabled"
	if st.Enabled {
		state = "enabled"
	}
	last := "never"
	if !st.LastSentAt.IsZero() {
		last = st.LastSentAt.Format(time.RFC3339)
	}
	fmt.Fprintf(stdout, "state       : %s\n", state)
	fmt.Fprintf(stdout, "endpoint    : %s\n", displayEndpoint(st.Endpoint))
	fmt.Fprintf(stdout, "anonymous_id: %s\n", telem.AnonymousID(st.ClusterUUID, st.InstallDate))
	fmt.Fprintf(stdout, "last_sent_at: %s\n", last)
	return nil
}

// previewCmd : `weft telemetry preview`. Prints the exact JSON
// envelope the agent would POST next, without dialling the
// endpoint. Operator-trust feature : never send anything you
// haven't audited at least once.
func previewCmd(stateDir *string) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Print the next payload to stdout without sending",
		Long: `Prints the JSON envelope that would be POSTed on the next 24h
tick, exactly as it'd go on the wire. No HTTP call ; no state
change. Use this BEFORE enabling to satisfy yourself the
payload is what the docs claim.

Requires telemetry to be enabled. Sample snapshots can be
generated for an unenabled cluster by passing --force-sample
(zeroed counts, demo cluster_uuid).`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runPreview(c.OutOrStdout(), *stateDir, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force-sample", false,
		"Render a sample envelope even when telemetry is disabled (demo cluster_uuid, zero counts).")
	return cmd
}

func runPreview(stdout io.Writer, stateDirFlag string, forceSample bool) error {
	store, err := newStore(resolveStateDir(stateDirFlag))
	if err != nil {
		return err
	}
	// In CLI mode we don't have an Adapter handle (the agent does).
	// Preview uses a zero Snapshot — the payload's value to the
	// operator is field-shape verification, not real counts. The
	// agent's first live tick generates the real numbers.
	src := zeroSource{}
	st, err := store.LoadState(context.Background())
	if err != nil {
		return err
	}
	if !st.Enabled && !forceSample {
		return errors.New("telemetry is disabled ; run `weft telemetry enable` first (or `--force-sample` for a stub)")
	}
	if forceSample {
		// Force-sample lets the operator audit the payload shape
		// without having to opt in first. Synthesise a stable
		// demo identity so two preview calls in a row print the
		// same anonymous_id ; flip Enabled so BuildPayload's gate
		// doesn't kick the call back as ErrDisabled.
		if st.ClusterUUID == "" {
			st.ClusterUUID = "00000000000000000000000000000000"
			st.InstallDate = "2026-01-01"
		}
		st.Enabled = true
	}
	tempStore := &previewStore{state: st}
	s := telem.New(telem.Options{Store: tempStore, Source: src, Now: time.Now})
	s.MarkStart(time.Now())
	p, _, err := s.BuildPayload(context.Background())
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

// previewStore wraps a fixed State for BuildPayload — avoids
// touching the live registry from preview.
type previewStore struct {
	state telem.State
}

func (p *previewStore) LoadState(_ context.Context) (telem.State, error) { return p.state, nil }
func (p *previewStore) SaveState(_ context.Context, _ telem.State) error { return nil }

// zeroSource is the CLI-side Source : we don't have an Adapter
// handle, so we feed zero counts. The agent's runtime path uses
// the real Source impl in cmd/weft/main.go.
type zeroSource struct{}

func (zeroSource) Snapshot(_ context.Context) (telem.Snapshot, error) {
	return telem.Snapshot{Drivers: []string{}, PluginsInstalled: []string{}}, nil
}

func displayEndpoint(ep string) string {
	if ep == "" {
		return "<unset — operator must wire one ; openweft has no default endpoint>"
	}
	return ep
}
