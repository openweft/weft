package pod

// get_spec.go implements `weft pod get-spec` — read the currently-
// published PodSpec for the given pod_id back from the agent and
// print it (as protojson) to stdout. Returns a clear "not published
// yet" message when no spec exists, exit code 1 so scripts can
// branch on it.

import (
	"context"
	"fmt"
	"os"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func getSpecCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var podID string
	cmd := &cobra.Command{
		Use:   "get-spec",
		Short: "Print the currently-published PodSpec for the given pod_id",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if podID == "" {
				return fmt.Errorf("--pod-id is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetPodSpec(context.Background(), &weftv1.GetPodSpecRequest{
				PodId: podID,
			})
			if err != nil {
				return err
			}
			if !resp.Found {
				fmt.Fprintf(os.Stderr, "pod %s : no spec published\n", podID)
				os.Exit(1)
			}
			// SpecJson is already a protojson byte slice ; print it
			// verbatim with a trailing newline.
			fmt.Printf("%s\n", string(resp.SpecJson))
			return nil
		},
	}
	cmd.Flags().StringVar(&podID, "pod-id", "", "Pod identifier (matches GuestHello.pod_id in the microVM)")
	return cmd
}
