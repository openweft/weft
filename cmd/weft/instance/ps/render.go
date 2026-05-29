package ps

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	introspectv1 "github.com/openweft/weft-proto/introspectv1"
)

// renderTable prints the process list in a `ps aux`-style aligned table.
func renderTable(w io.Writer, procs []*introspectv1.Process) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tPID\tPPID\t%CPU\t%MEM\tVSZ\tRSS\tTTY\tSTAT\tSTART\tCOMMAND")
	for _, p := range procs {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.1f\t%.1f\t%d\t%d\t%s\t%s\t%s\t%s\n",
			p.User, p.Pid, p.Ppid, p.CpuPercent, p.MemPercent,
			p.VszKb, p.RssKb, ttyOrDash(p.Tty), p.State,
			startTime(p.StartTimeMs), p.Command,
		)
	}
	return tw.Flush()
}

// renderJSON prints one JSON object per process, newline-delimited —
// matching the JSON shape the other instance subcommands emit.
func renderJSON(w io.Writer, procs []*introspectv1.Process) error {
	for _, p := range procs {
		if _, err := fmt.Fprintf(w,
			"{\"pid\":%d,\"ppid\":%d,\"user\":%q,\"state\":%q,\"cpu_percent\":%.2f,\"mem_percent\":%.2f,\"vsz_kb\":%d,\"rss_kb\":%d,\"tty\":%q,\"start_time_ms\":%d,\"command\":%q}\n",
			p.Pid, p.Ppid, p.User, p.State, p.CpuPercent, p.MemPercent,
			p.VszKb, p.RssKb, p.Tty, p.StartTimeMs, p.Command,
		); err != nil {
			return err
		}
	}
	return nil
}

func ttyOrDash(tty string) string {
	if tty == "" {
		return "?"
	}
	return tty
}

// startTime renders an epoch-ms start as "Jan02 15:04", or "?" when unset.
func startTime(ms int64) string {
	if ms <= 0 {
		return "?"
	}
	return time.UnixMilli(ms).Format("Jan02 15:04")
}
