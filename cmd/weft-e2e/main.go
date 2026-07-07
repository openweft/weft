// Package main implements `weft-e2e` — the cluster-level end-to-end
// test harness operators run against a real weft deployment (live
// 3-DC, single-host dev, anything in between).
//
// Why this exists : unit tests catch logic errors in isolation, but
// every cross-host bug we've debugged this week (placement collapse,
// OCI label drift, stale-zombie sticking, RestartVM falling into the
// local-default-project path) only surfaces when an RPC actually
// crosses DCs. The harness exercises the platform end-to-end against
// a live cluster + asserts the operator-visible invariants : "3
// replicas land on 3 distinct AZs", "RestartVM on a peer-owned VM
// dispatches to the right host", "uninstalling a plugin removes its
// VMs from all hosts", etc.
//
// Operator usage :
//
//	weft-e2e --socket ~/.weft/weft.sock                       # smoke, ~1 min
//	weft-e2e --socket ~/.weft/weft.sock --suite=full          # full, ~10 min
//	GH_TOKEN=$(...) weft-e2e --ssh-socket admin@dc1-r1-h1:... # over SSH
//
// Exit code 0 on full success ; non-zero on any assertion failure ;
// every test prints PASS/FAIL with a one-line summary at the end.
//
// Adding a test : drop a func(*Ctx) into the registry below. Use the
// Ctx helpers (require/expect/eventually) instead of plain panic so
// the runner can capture + continue.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	weftclient "github.com/openweft/weft-client"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

func main() {
	var (
		socket    string
		sshSocket string
		sshKey    string
		suite     string
		verbose   bool
		filter    string
	)
	root := &cobra.Command{
		Use:           "weft-e2e",
		Short:         "Cluster-level end-to-end test harness for a live weft deployment",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			client, conn, err := dial(socket, sshSocket, sshKey)
			if err != nil {
				return fmt.Errorf("dial: %w", err)
			}
			defer conn.Close()

			cases := pickSuite(suite, filter)
			if len(cases) == 0 {
				return fmt.Errorf("no tests matched suite=%q filter=%q", suite, filter)
			}

			runner := &Runner{
				Client:  client,
				Out:     os.Stdout,
				Verbose: verbose,
				Suite:   suite,
			}
			if rc := runner.Run(cases); rc != 0 {
				os.Exit(rc)
			}
			return nil
		},
	}
	f := root.Flags()
	f.StringVar(&socket, "socket", defaultSocket(), "weft agent Unix socket path (plain)")
	f.StringVar(&sshSocket, "ssh-socket", "", "user@host:/path/to/weft-ssh.sock for SSH transport")
	f.StringVar(&sshKey, "ssh-key", "", "SSH private key when --ssh-socket is set")
	f.StringVar(&suite, "suite", "smoke", "test suite : smoke (quick invariants) | full (smoke + plugin lifecycle + cross-host RPCs)")
	f.BoolVarP(&verbose, "verbose", "v", false, "print each test's progress lines, not just PASS/FAIL")
	f.StringVar(&filter, "run", "", "regex-free substring filter — only tests whose name contains this run")

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "weft-e2e: %v\n", err)
		os.Exit(2)
	}
}

// dial selects the transport based on the flag combo. Mirrors the
// CLI's shared.Client : --ssh-socket switches to the SSH tunnel,
// otherwise the plain Unix socket. Keeping the wiring in one place
// avoids drift from the operator CLI.
func dial(socket, sshSocket, sshKey string) (weftv1.WeftAgentClient, *grpc.ClientConn, error) {
	if sshSocket != "" {
		conn, err := weftclient.Dial(socket, weftclient.WithSSH(sshSocket, sshKey))
		if err != nil {
			return nil, nil, err
		}
		return weftv1.NewWeftAgentClient(conn), conn, nil
	}
	return weftclient.Client(socket)
}

func defaultSocket() string {
	if v := os.Getenv("WEFT_SOCKET"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return home + "/.weft/weft.sock"
}

// pickSuite returns the ordered list of tests for a suite + filter
// combo. Smoke is always a subset of full — full just appends the
// longer scenarios. Filter is a plain substring match against
// the case Name so operators can re-run a single failing test
// without flag gymnastics.
func pickSuite(suite, filter string) []Case {
	var out []Case
	for _, c := range allCases {
		if c.Suite == "smoke" || suite == "full" {
			if filter == "" || strings.Contains(c.Name, filter) {
				out = append(out, c)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Order < out[j].Order
	})
	return out
}

// Runner walks the case list, invokes each test, captures pass/fail
// + duration, prints a summary, and returns the exit code.
type Runner struct {
	Client  weftv1.WeftAgentClient
	Out     io.Writer
	Verbose bool
	Suite   string
}

func (r *Runner) Run(cases []Case) int {
	fmt.Fprintf(r.Out, "weft-e2e suite=%s tests=%d\n", r.Suite, len(cases))
	var pass, fail, skip int
	failed := []string{}
	t0 := time.Now()
	for _, c := range cases {
		ctx := &Ctx{
			Client:  r.Client,
			Out:     r.Out,
			Verbose: r.Verbose,
			Name:    c.Name,
		}
		start := time.Now()
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					ctx.failed = true
					ctx.lastErr = fmt.Sprintf("PANIC: %v", rec)
				}
			}()
			c.Fn(ctx)
		}()
		dur := time.Since(start).Truncate(time.Millisecond)
		switch {
		case ctx.skipped:
			skip++
			fmt.Fprintf(r.Out, "  SKIP %-40s  %s  %s\n", c.Name, dur, ctx.lastErr)
		case ctx.failed:
			fail++
			failed = append(failed, c.Name)
			fmt.Fprintf(r.Out, "  FAIL %-40s  %s  %s\n", c.Name, dur, ctx.lastErr)
		default:
			pass++
			fmt.Fprintf(r.Out, "  PASS %-40s  %s\n", c.Name, dur)
		}
	}
	fmt.Fprintf(r.Out, "\nweft-e2e total=%s pass=%d fail=%d skip=%d\n",
		time.Since(t0).Truncate(time.Millisecond), pass, fail, skip)
	if fail > 0 {
		fmt.Fprintf(r.Out, "failed : %s\n", strings.Join(failed, ", "))
		return 1
	}
	return 0
}

// Case describes one e2e scenario : a name, the suite it belongs to
// (smoke runs by default ; full adds the longer ones), an ordering
// hint so deterministic-order tests don't fight with parallel-safe
// ones, and the function the runner invokes.
type Case struct {
	Name  string
	Suite string // "smoke" or "full"
	Order int    // ascending ; same value = no relative guarantee
	Fn    func(*Ctx)
}

// Ctx is the per-test context passed to every Fn. Mirrors the parts
// of testing.T we actually need : require (terminal failure), expect
// (non-terminal failure, keep running), Eventually (poll until
// success or timeout), logf (verbose-mode progress).
type Ctx struct {
	Client  weftv1.WeftAgentClient
	Out     io.Writer
	Verbose bool
	Name    string

	failed  bool
	skipped bool
	lastErr string
}

// require records a fatal failure + bails out of the test via panic
// (the runner catches it). Call this when continuing wouldn't make
// sense — e.g. ListHosts returned an error so no follow-up assertion
// can run.
func (c *Ctx) require(condition bool, format string, args ...any) {
	if condition {
		return
	}
	c.failed = true
	c.lastErr = fmt.Sprintf(format, args...)
	panic("require failed: " + c.lastErr)
}

// expect records a non-fatal failure ; the test continues so the
// caller can collect every assertion that breaks in one run. Returns
// `condition` so the caller can chain `if !ctx.expect(...) { ... }`
// when the next step depends on this one.
func (c *Ctx) expect(condition bool, format string, args ...any) bool {
	if condition {
		return true
	}
	c.failed = true
	c.lastErr = fmt.Sprintf(format, args...)
	return false
}

// logf prints a progress line, gated on -v. Tests use this to leave
// breadcrumbs ("found 6 hosts", "redis-ha install kicked off") so a
// debugging run can trace what happened without re-instrumenting.
func (c *Ctx) logf(format string, args ...any) {
	if !c.Verbose {
		return
	}
	fmt.Fprintf(c.Out, "    [%s] %s\n", c.Name, fmt.Sprintf(format, args...))
}

// eventually polls fn() at 1Hz until it returns true OR timeout
// elapses. Returns true when the condition met, false on timeout.
// Tests use this for any "the cluster should converge to X" check
// (state transitions, eventual consistency over etcd watches, …)
// without spreading sleep() everywhere.
func (c *Ctx) eventually(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(1 * time.Second)
	}
}

// skip flags the test as skipped (e.g. preconditions not met — no
// plugin catalogue loaded, single-host cluster but test needs 3+).
// Skips don't count toward the failure total.
func (c *Ctx) skip(reason string) {
	c.skipped = true
	c.lastErr = reason
	panic("skip: " + reason)
}

// background returns a ctx with the supplied timeout — saves every
// test from re-typing the same boilerplate.
func bg(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}
