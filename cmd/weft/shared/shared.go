// Package shared provides output renderers shared by all vzc
// sub-commands. The gRPC-dial / state-string / human-bytes helpers
// live in the external `github.com/openweft/weft-client` module so
// vzc and ncl agree on dial behaviour, state names, and byte
// formatting. This package re-exports the bits vzc's existing call
// sites refer to (Dial / Client / ProtoStateStr / HumanBytes) so
// the cobra command files keep working unchanged.
package shared

import (
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft-client"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"google.golang.org/grpc"

	"fmt"
	"os"
)

// Dial opens a gRPC connection to vzd. Uses SSH transport when sshKey is set.
// Thin wrapper kept for source compatibility with existing call sites; new
// code should use vzclient.Dial(...) directly.
func Dial(socketPath, sshSocket, sshKey string) (*grpc.ClientConn, error) {
	var opts []vzclient.Option
	if sshKey != "" {
		opts = append(opts, vzclient.WithSSH(sshSocket, sshKey))
	}
	return vzclient.Dial(socketPath, opts...)
}

// Client creates a gRPC connection and returns the VzdServiceClient.
// Thin wrapper over vzclient.Client.
func Client(socketPath, sshSocket, sshKey string) (vzdv1.VzdServiceClient, *grpc.ClientConn, error) {
	var opts []vzclient.Option
	if sshKey != "" {
		opts = append(opts, vzclient.WithSSH(sshSocket, sshKey))
	}
	return vzclient.Client(socketPath, opts...)
}

// RenderTable prints VM infos as a table to stdout.
func RenderTable(vms []*vzdv1.VMInfo) {
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
func PrintJSON(vms []*vzdv1.VMInfo) error {
	for _, vm := range vms {
		fmt.Printf("{\"name\":%q,\"state\":%q,\"os\":%q,\"cpu\":%d,\"mem_mb\":%d,\"disk_gb\":%d,\"ip\":%q}\n",
			vm.Name, ProtoStateStr(vm.State), vm.Os, vm.Cpu, vm.MemMb, vm.DiskGb, vm.Ip)
	}
	return nil
}

// RenderImagesTable prints image infos as a table to stdout.
func RenderImagesTable(images []*vzdv1.ImageInfo) {
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
func PrintImagesJSON(images []*vzdv1.ImageInfo) error {
	for _, img := range images {
		fmt.Printf("{\"url\":%q,\"name\":%q,\"format\":%q,\"size_bytes\":%d}\n",
			img.Url, img.Name, img.Format, img.SizeBytes)
	}
	return nil
}

// HumanBytes formats a byte count as a human-readable string.
// Thin wrapper kept for source compatibility — delegates to
// vzclient.HumanBytes so vzc and ncl format sizes identically.
func HumanBytes(b int64) string { return vzclient.HumanBytes(b) }

// ProtoStateStr converts a proto VMState enum to a human-readable
// label. Thin wrapper over vzclient.StateString.
func ProtoStateStr(s vzdv1.VMState) string { return vzclient.StateString(s) }
