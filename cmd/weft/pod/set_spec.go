package pod

// set_spec.go implements `weft pod set-spec` — read a protojson-
// encoded guestv1.PodSpec from a file (or stdin) and publish it to
// the agent for the given pod_id. The agent decodes, stores it in
// the in-memory registry, and persists the whole registry to
// <stateDir>/podspecs.hcl atomically.
//
// Mirrors the kubectl-apply-with-file ergonomic : the file is the
// source of truth ; --pod-id specifies which pod the spec applies
// to. An empty --from-file (or a literal `-` meaning stdin) lets
// scripts pipe a generated spec without hitting disk.

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func setSpecCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		podID    string
		fromFile string
	)
	cmd := &cobra.Command{
		Use:   "set-spec",
		Short: "Publish a PodSpec (protojson) for the given pod_id",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if podID == "" {
				return fmt.Errorf("--pod-id is required")
			}
			var raw []byte
			var err error
			switch fromFile {
			case "":
				return fmt.Errorf("--from-file is required (pass `-` to read stdin)")
			case "-":
				raw, err = io.ReadAll(os.Stdin)
			default:
				raw, err = os.ReadFile(fromFile)
			}
			if err != nil {
				return fmt.Errorf("read spec: %w", err)
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.SetPodSpec(context.Background(), &weftv1.SetPodSpecRequest{
				PodId:    podID,
				SpecJson: raw,
			})
			if err != nil {
				return err
			}
			fmt.Printf("pod %s : spec published (%d bytes)\n", podID, len(raw))
			return nil
		},
	}
	cmd.Flags().StringVar(&podID, "pod-id", "", "Pod identifier (matches GuestHello.pod_id in the microVM)")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "Path to a protojson-encoded guestv1.PodSpec (use `-` for stdin)")
	return cmd
}
