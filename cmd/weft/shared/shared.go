// Package shared provides output renderers shared by all weft
// sub-commands. The gRPC-dial / state-string / human-bytes helpers
// live in the external `github.com/openweft/weft-client` module so
// weft and weft-microvm agree on dial behaviour, state names, and byte
// formatting. This package re-exports the bits weft's existing call
// sites refer to (Dial / Client / ProtoStateStr / HumanBytes) so
// the cobra command files keep working unchanged.
package shared

import (
	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft-client"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"google.golang.org/grpc"

	"fmt"
	"io"
	"os"
	"strings"
)

// Dial opens a gRPC connection to weft. Uses SSH transport when sshKey is set.
// Thin wrapper kept for source compatibility with existing call sites; new
// code should use weftclient.Dial(...) directly.
func Dial(socketPath, sshSocket, sshKey string) (*grpc.ClientConn, error) {
	var opts []weftclient.Option
	if sshKey != "" {
		opts = append(opts, weftclient.WithSSH(sshSocket, sshKey))
	}
	return weftclient.Dial(socketPath, opts...)
}

// Client creates a gRPC connection and returns the WeftAgentClient.
// Thin wrapper over weftclient.Client.
func Client(socketPath, sshSocket, sshKey string) (weftv1.WeftAgentClient, *grpc.ClientConn, error) {
	var opts []weftclient.Option
	if sshKey != "" {
		opts = append(opts, weftclient.WithSSH(sshSocket, sshKey))
	}
	return weftclient.Client(socketPath, opts...)
}

// RenderTable prints VM infos as a table to stdout.
func RenderTable(vms []*weftv1.VMInfo) {
	headers := []string{"Name", "State", "OS", "CPU", "Mem (MB)", "Disk (GB)", "IP"}
	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithHeader(headers),
		tablewriter.WithHeaderAutoWrap(tw.WrapNone),
	)
	for _, vm := range vms {
		_ = table.Append([]string{
			vm.Name,
			ProtoStateStr(vm.State),
			vm.Os,
			fmt.Sprintf("%d", vm.Cpu),
			fmt.Sprintf("%d", vm.MemMb),
			fmt.Sprintf("%d", vm.DiskGb),
			vm.Ip,
		})
	}
	_ = table.Render()
}

// PrintJSON marshals the VM list to JSON lines on stdout.
func PrintJSON(vms []*weftv1.VMInfo) error {
	return PrintJSONTo(os.Stdout, vms)
}

// PrintJSONTo writes the VM list as JSON lines to w. Useful for
// admin tools that wrap PrintJSON behind a `-o file` flag.
func PrintJSONTo(w io.Writer, vms []*weftv1.VMInfo) error {
	for _, vm := range vms {
		if _, err := fmt.Fprintf(w,
			"{\"name\":%q,\"uuid\":%q,\"project_uuid\":%q,\"state\":%q,\"os\":%q,\"cpu\":%d,\"mem_mb\":%d,\"disk_gb\":%d,\"ip\":%q,\"properties\":%s}\n",
			vm.Name, vm.Uuid, vm.ProjectUuid, ProtoStateStr(vm.State), vm.Os,
			vm.Cpu, vm.MemMb, vm.DiskGb, vm.Ip, jsonProperties(vm.Properties),
		); err != nil {
			return err
		}
	}
	return nil
}

// jsonProperties emits a string-string map as a JSON object. Falls back
// to "{}" on empty / nil so consumers don't have to special-case it.
func jsonProperties(in map[string]string) string {
	if len(in) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(in))
	for k, v := range in {
		parts = append(parts, fmt.Sprintf("%q:%q", k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// RenderImagesTable prints image infos as a table to stdout.
func RenderImagesTable(images []*weftv1.ImageInfo) {
	headers := []string{"Name", "Format", "URL", "Size"}
	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithHeader(headers),
		tablewriter.WithHeaderAutoWrap(tw.WrapNone),
	)
	for _, img := range images {
		_ = table.Append([]string{
			img.Name,
			img.Format,
			img.Url,
			HumanBytes(img.SizeBytes),
		})
	}
	_ = table.Render()
}

// PrintImagesJSON prints image infos as JSON lines on stdout.
func PrintImagesJSON(images []*weftv1.ImageInfo) error {
	for _, img := range images {
		fmt.Printf("{\"url\":%q,\"name\":%q,\"format\":%q,\"size_bytes\":%d}\n",
			img.Url, img.Name, img.Format, img.SizeBytes)
	}
	return nil
}

// HumanBytes formats a byte count as a human-readable string.
// Thin wrapper kept for source compatibility — delegates to
// weftclient.HumanBytes so weft and weft-microvm format sizes identically.
func HumanBytes(b int64) string { return weftclient.HumanBytes(b) }

// ProtoStateStr converts a proto VMState enum to a human-readable
// label. Thin wrapper over weftclient.StateString.
func ProtoStateStr(s weftv1.VMState) string { return weftclient.StateString(s) }
