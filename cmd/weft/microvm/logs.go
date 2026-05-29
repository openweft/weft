// logs.go implements `weft microvm logs` — the Docker-`logs` analogue
// for microVMs.
//
// The serial console captured by the agent at <vmDir>/console.log
// mixes four sources: kernel boot messages, ncl-init log lines,
// NCL_MARK markers, and the container's own stdout/stderr. By default
// this verb returns only the last bucket — what the user's process
// actually printed — by slicing the byte stream between the
// `NCL_MARK exec_ready` and `NCL_MARK child_exited` markers and
// stripping ncl-init / NCL_MARK lines inside that window. `--raw`
// disables the filter and dumps the whole stream (same as
// `weft instance logs`).
package microvm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// logsCmd returns the `weft microvm logs` command.
func logsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		raw     bool
		tail    int64
		project string
	)
	cmd := &cobra.Command{
		Use:   "logs NAME",
		Short: "Show the container output (or raw serial console) for a microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			vmName := resolveName(args[0])
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := c.VMLogs(ctx, &vzdv1.VMLogsRequest{
				Name:      vmName,
				Project:   project,
				TailBytes: tail,
			})
			if err != nil {
				return err
			}
			if len(resp.Contents) == 0 {
				return nil
			}
			out := resp.Contents
			if !raw {
				out = containerOutput(out)
			}
			if _, err := os.Stdout.Write(out); err != nil {
				return err
			}
			// Ensure a trailing newline so a follow-up shell prompt
			// isn't glued to the container's last byte.
			if len(out) > 0 && out[len(out)-1] != '\n' {
				fmt.Fprintln(os.Stdout)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "dump the raw serial console (no filtering)")
	cmd.Flags().Int64Var(&tail, "tail", 0, "cap the response to the last N bytes (0 = whole file)")
	cmd.Flags().StringVar(&project, "project", "", "project namespace")
	return cmd
}

// containerOutput slices the byte stream between the
// `NCL_MARK exec_ready` and `NCL_MARK child_exited` markers and drops
// any ncl-init / NCL_MARK lines that landed inside the window. If
// exec_ready is absent (the VM never executed the entrypoint), returns
// an empty slice — better than dumping the boot transcript when the
// user asked for "container output".
func containerOutput(b []byte) []byte {
	const startMark = "NCL_MARK exec_ready"
	const endMark = "NCL_MARK child_exited"
	startIdx := bytes.Index(b, []byte(startMark))
	if startIdx < 0 {
		return nil
	}
	// Advance past the marker line itself.
	if nl := bytes.IndexByte(b[startIdx:], '\n'); nl >= 0 {
		startIdx += nl + 1
	} else {
		return nil
	}
	endIdx := bytes.Index(b[startIdx:], []byte(endMark))
	var window []byte
	if endIdx < 0 {
		window = b[startIdx:]
	} else {
		window = b[startIdx : startIdx+endIdx]
	}
	scanner := bufio.NewScanner(bytes.NewReader(window))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var buf bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "NCL_MARK ") {
			continue
		}
		if strings.HasPrefix(line, "ncl-init: ") {
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}
