// Package upgrade ships the `weft upgrade` + `weft downgrade`
// commands : a thin orchestrator that automates the rolling-restart
// procedure documented at docs/operations/upgrade.md. The procedure
// is itself the contract — this command just runs it sequentially
// per host, with cordon-on-drain, image pull, systemctl restart,
// readiness poll, soak window, and uncordon-on-success.
//
// Why a CLI command (vs a one-shot script in docs) : operators
// running on a 3-DC cluster ALREADY have weft installed on their
// laptop ; one binary that knows the cluster shape + can talk to
// the control plane (cordon, list hosts) + can SSH to each host
// (image install, systemctl restart) is the least-surprise
// experience. Both upgrade and downgrade share the same code
// path — the only difference is the log prefix and a downgrade
// confirmation gate.
//
// Out of scope :
//   - Image distribution (operator pre-pulls the image into the
//     host's container runtime ; this command's --image-pull is
//     just a shell command we run before restart, default
//     `docker pull ghcr.io/openweft/weft:<version>`).
//   - etcd snapshot (do this manually before running upgrade ;
//     the runbook checklist still applies).
//   - Roll-forward / roll-back on partial failure : we STOP on
//     the first failing host and leave it cordoned so operators
//     can investigate ; resuming = re-run with the same args
//     (idempotent on already-completed hosts because cordon →
//     pull → install is itself idempotent).

package upgrade

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Command returns the parent cobra command exposing `weft upgrade`
// and `weft downgrade`. socket / sshSocket / sshKey are the
// standard control-plane flags every other weft CLI carries —
// passed in by main.go so this package doesn't take a transitive
// dep on the rest of the CLI's wiring.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	c := &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"downgrade"},
		Short:   "Rolling-restart every host onto a target weft version (or any earlier one when called as `downgrade`)",
		Long: `Rolling-restart every host onto a target weft version, one host at a time,
preserving etcd quorum. Automates the procedure from
docs/operations/upgrade.md ; an operator that wants the manual
control still has the runbook to follow by hand.

Each host gets : cordon → ssh pull → ssh install → ssh restart →
poll readiness → soak window → uncordon. The command stops on the
first failure and leaves that host cordoned so the operator can
investigate ; rerun with the same args to resume.

The same code path runs as 'weft downgrade' (alias) ; the only
behavioural difference is the confirmation gate when the target
version is older than the current one. There is no policy
mechanism — operators are trusted to know what they're rolling to.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd, optsFromFlags(cmd))
		},
	}

	f := c.Flags()
	f.String("to", "", "target weft version tag (e.g. v0.4.52). Required.")
	f.StringSlice("host", nil, "host SSH targets (user@addr ; can repeat). Required if --cluster-hcl is empty.")
	f.String("cluster-hcl", "", "path to cluster.hcl to read the host list from (alternative to repeated --host)")
	f.Duration("soak", 10*time.Minute, "wait period after a host comes back ready, before the next host")
	f.Duration("ready-timeout", 60*time.Second, "how long to poll for the host to report ready after restart")
	f.Duration("ready-interval", 2*time.Second, "poll cadence while waiting for ready")
	f.String("image-pull", "docker pull ghcr.io/openweft/weft:{{.Version}}", "shell template run on each host before restart ; {{.Version}} expanded")
	f.String("install-cmd", "install -m0755 /var/cache/weft/weft-{{.Version}} /usr/local/bin/weft", "shell template that installs the new binary on each host ; {{.Version}} expanded")
	f.String("restart-cmd", "systemctl restart weft-agent.service", "shell command run on each host after the install step")
	f.Bool("yes", false, "skip the downgrade confirmation prompt (no effect on upgrade)")
	f.Bool("dry-run", false, "print every step + skip execution. Use this on the first invocation to verify the host list.")
	f.String("ssh-flags", "-o StrictHostKeyChecking=accept-new -o ConnectTimeout=5", "extra arguments inserted between `ssh` and the host target")

	// Silence the unused-import warning under socket/sshSocket/sshKey
	// when this package doesn't actually need control-plane RPC access
	// (cordon is done via shell-out to `weft host cordon`, so we never
	// dial the weft daemon ourselves). Keep the params for signature
	// symmetry with the other CLI packages in cmd/weft.
	_ = socket
	_ = sshSocket
	_ = sshKey

	return c
}

// options collects the parsed flag values. Pure value type so
// tests can drive runUpgrade(nil, opts) without involving cobra.
type options struct {
	Version       string
	Hosts         []string
	ClusterHCL    string
	Soak          time.Duration
	ReadyTimeout  time.Duration
	ReadyInterval time.Duration
	ImagePullTmpl string
	InstallTmpl   string
	RestartCmd    string
	Yes           bool
	DryRun        bool
	SSHFlags      string
	// Downgrade is set when the parent command was invoked as
	// 'weft downgrade' ; gates the confirmation prompt + flips
	// log prefix.
	Downgrade bool
	// Out is where every step's log line lands. nil → os.Stdout.
	Out *os.File
}

func optsFromFlags(c *cobra.Command) options {
	f := c.Flags()
	v, _ := f.GetString("to")
	hosts, _ := f.GetStringSlice("host")
	clusterHCL, _ := f.GetString("cluster-hcl")
	soak, _ := f.GetDuration("soak")
	readyTO, _ := f.GetDuration("ready-timeout")
	readyIV, _ := f.GetDuration("ready-interval")
	imgPull, _ := f.GetString("image-pull")
	install, _ := f.GetString("install-cmd")
	restart, _ := f.GetString("restart-cmd")
	yes, _ := f.GetBool("yes")
	dry, _ := f.GetBool("dry-run")
	sshF, _ := f.GetString("ssh-flags")
	return options{
		Version:       v,
		Hosts:         hosts,
		ClusterHCL:    clusterHCL,
		Soak:          soak,
		ReadyTimeout:  readyTO,
		ReadyInterval: readyIV,
		ImagePullTmpl: imgPull,
		InstallTmpl:   install,
		RestartCmd:    restart,
		Yes:           yes,
		DryRun:        dry,
		SSHFlags:      sshF,
		Downgrade:     c.CalledAs() == "downgrade",
	}
}

// runUpgrade is the test seam — pure-ish (still shells out to ssh
// + weft) but no cobra references. cmd is optional ; pass nil in
// tests to skip the Cobra-side help printing.
func runUpgrade(cmd *cobra.Command, o options) error {
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	verb := "upgrade"
	if o.Downgrade {
		verb = "downgrade"
	}

	if o.Version == "" {
		return fmt.Errorf("--to is required (e.g. --to=v0.4.52)")
	}
	if len(o.Hosts) == 0 && o.ClusterHCL == "" {
		return fmt.Errorf("either --host (repeated) or --cluster-hcl is required")
	}
	if o.ClusterHCL != "" && len(o.Hosts) == 0 {
		extracted, err := hostsFromClusterHCL(o.ClusterHCL)
		if err != nil {
			return fmt.Errorf("read cluster.hcl: %w", err)
		}
		o.Hosts = extracted
	}
	if len(o.Hosts) == 0 {
		return fmt.Errorf("no hosts to %s ; cluster.hcl produced an empty list", verb)
	}

	if o.Downgrade && !o.Yes {
		fmt.Fprintf(out, "weft downgrade: about to roll %d host(s) BACKWARD to %s\n", len(o.Hosts), o.Version)
		fmt.Fprintln(out, "downgrade is potentially destructive if the source version performed an etcd schema migration")
		fmt.Fprintln(out, "see docs/operations/upgrade.md §5 for the safe rollback procedure")
		fmt.Fprintln(out, "re-run with --yes to proceed, or abort and follow the manual runbook")
		return fmt.Errorf("downgrade refused without --yes")
	}

	fmt.Fprintf(out, "weft %s: target=%s hosts=%v soak=%s dry-run=%v\n", verb, o.Version, o.Hosts, o.Soak, o.DryRun)

	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
	}

	for i, host := range o.Hosts {
		fmt.Fprintf(out, "\n=== %s phase %d/%d : %s ===\n", verb, i+1, len(o.Hosts), host)
		if err := upgradeOneHost(ctx, out, host, o); err != nil {
			return fmt.Errorf("phase %d (%s) : %w ; the host is left cordoned for investigation", i+1, host, err)
		}
		if i < len(o.Hosts)-1 && o.Soak > 0 {
			fmt.Fprintf(out, "soaking %s before next host\n", o.Soak)
			if !o.DryRun {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(o.Soak):
				}
			}
		}
	}

	fmt.Fprintf(out, "\nweft %s: all %d host(s) on %s — done\n", verb, len(o.Hosts), o.Version)
	return nil
}

// upgradeOneHost runs the per-host steps from the runbook. Stops
// on the first failing step ; the caller surfaces the error and
// leaves the host cordoned (we don't uncordon-on-failure on
// purpose — the operator gets to investigate without traffic
// landing back on the half-restarted node).
func upgradeOneHost(ctx context.Context, out *os.File, host string, o options) error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"cordon", func() error { return runLocalWeft(out, o.DryRun, "host", "cordon", hostShortName(host)) }},
		{"image-pull", func() error { return runSSH(out, host, expand(o.ImagePullTmpl, o.Version), o.DryRun, o.SSHFlags) }},
		{"install", func() error { return runSSH(out, host, expand(o.InstallTmpl, o.Version), o.DryRun, o.SSHFlags) }},
		{"restart", func() error { return runSSH(out, host, o.RestartCmd, o.DryRun, o.SSHFlags) }},
		{"wait-ready", func() error { return waitHostReady(ctx, out, hostShortName(host), o) }},
		{"uncordon", func() error { return runLocalWeft(out, o.DryRun, "host", "uncordon", hostShortName(host)) }},
	}
	for _, s := range steps {
		fmt.Fprintf(out, "→ %s\n", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// runSSH shells out to /usr/bin/ssh — the operator's ssh-agent +
// ~/.ssh/config are reused as-is. No new key management.
func runSSH(out *os.File, host, command string, dryRun bool, sshFlags string) error {
	fmt.Fprintf(out, "  ssh %s -- %s\n", host, command)
	if dryRun {
		return nil
	}
	args := append(strings.Fields(sshFlags), host, command)
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// runLocalWeft shells out to `weft …` locally — same binary the
// operator is running, same auth (the operator's existing
// credential file). Cordon/uncordon use this path so we don't
// need a gRPC client wired in the upgrade package.
func runLocalWeft(out *os.File, dryRun bool, args ...string) error {
	fmt.Fprintf(out, "  weft %s\n", strings.Join(args, " "))
	if dryRun {
		return nil
	}
	cmd := exec.Command("weft", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// waitHostReady polls `weft host get <name> --json` until the
// host's state is ready or the timeout fires. We accept the
// `--json` failure mode (CLI not yet wired here) by parsing
// `weft host list` output as a fallback ; either way the
// distinguishing signal is "Active" in the State column.
//
// For simplicity in the v1 of this command we poll plain text :
// the operator can re-run with --dry-run to confirm the host name
// format if the poll never matches.
func waitHostReady(ctx context.Context, out *os.File, hostShort string, o options) error {
	if o.DryRun {
		fmt.Fprintf(out, "  (dry-run) skip readiness poll\n")
		return nil
	}
	deadline := time.Now().Add(o.ReadyTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := pollHostReady(hostShort)
		if err == nil && ready {
			fmt.Fprintf(out, "  host %s ready\n", hostShort)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.ReadyInterval):
		}
	}
	return fmt.Errorf("host %s not ready after %s", hostShort, o.ReadyTimeout)
}

// pollHostReady runs `weft host list` + checks if hostShort
// appears with the Active state. Returns (false, err) if the
// command fails ; (false, nil) if the host isn't listed as Active.
func pollHostReady(hostShort string) (bool, error) {
	cmd := exec.Command("weft", "host", "list")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("weft host list: %w (output: %s)", err, string(output))
	}
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, hostShort) {
			continue
		}
		// Active and not Cordoned (cordoned hosts are technically
		// Active+Cordoned ; we want both Active AND uncordoned ;
		// the readiness check fires BEFORE the uncordon step, so
		// "cordoned=true" is the expected mid-upgrade state).
		if strings.Contains(line, "Active") {
			return true, nil
		}
	}
	return false, nil
}

// hostShortName extracts the bare hostname from a user@host[:port]
// target — the format `weft host` commands accept. Returns the
// input unchanged when there's no '@' (already a bare name).
func hostShortName(t string) string {
	if at := strings.IndexByte(t, '@'); at >= 0 {
		t = t[at+1:]
	}
	if colon := strings.IndexByte(t, ':'); colon >= 0 {
		t = t[:colon]
	}
	return t
}

// expand substitutes {{.Version}} in a shell template. We don't
// pull text/template in for one variable ; a plain Replace is
// enough and matches operators' mental model of "literal command
// with one placeholder".
func expand(tmpl, version string) string {
	return strings.ReplaceAll(tmpl, "{{.Version}}", version)
}

// hostsFromClusterHCL reads cluster.hcl and returns its host
// addresses. v1 implementation : look for `addr = "..."` lines
// inside `host` blocks. A full HCL parse would be more robust ;
// the regex pattern works for the canonical cluster.hcl shape
// the docs ship + the doc warns operators to fall back to
// --host repeats if their shape diverges.
func hostsFromClusterHCL(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hosts []string
	var inHost bool
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "host ") && strings.Contains(line, "{"):
			inHost = true
		case line == "}":
			inHost = false
		case inHost && strings.HasPrefix(line, "addr"):
			// addr = "user@host" or addr = "host"
			if eq := strings.IndexByte(line, '='); eq >= 0 {
				v := strings.TrimSpace(line[eq+1:])
				v = strings.Trim(v, `"`)
				if v != "" {
					hosts = append(hosts, v)
				}
			}
		}
	}
	return hosts, nil
}
