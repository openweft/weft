// Package registermicrovm implements the `vzc instance
// register-microvm` sub-command.
//
// It's the client-side counterpart of vzd's RegisterMicroVM RPC,
// added so nano-container-linux's `ncl run` can wire a microVM
// (boot.iso + virtio-fs shares of an extracted OCI rootfs) into
// vzd's inventory without bypassing the daemon. Once the call
// returns the VM is `STOPPED`; bring it up with `vzc instance start`.
package registermicrovm

import (
	"context"
	"fmt"
	"strings"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the register-microvm cobra command.
//
// CLI shape:
//
//	vzc instance register-microvm <name>
//	    --boot-iso <path>
//	    --share TAG=PATH[:ro]
//	    [--share ...]
//
// At least one `--share` is required (a microVM with no shares
// could just be CreateVM); the `:ro` suffix marks the share as
// read-only inside the guest. Tag conventions used by ncl: the
// OCI rootfs share is "rootfs0".
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		bootISO string
		kernel  string
		initrd  string
		cmdline string
		shares  []string
	)
	cmd := &cobra.Command{
		Use:   "register-microvm <name>",
		Short: "Register a microVM (boot.iso OR kernel+initrd, + virtio-fs shares) with vzd",
		Long: `Register a microVM directory with vzd. Two boot modes:

  - UKI mode:    --boot-iso PATH
  - Direct-Linux: --kernel PATH [--initrd PATH] [--cmdline "…"]

Exactly one of --boot-iso or --kernel must be set. At least one
--share TAG=PATH[:ro] is required (typical for ncl: a single share
rootfs0=/path/to/extracted/oci/rootfs).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if bootISO == "" && kernel == "" {
				return fmt.Errorf("either --boot-iso or --kernel is required")
			}
			if bootISO != "" && kernel != "" {
				return fmt.Errorf("--boot-iso and --kernel are mutually exclusive")
			}
			if len(shares) == 0 {
				return fmt.Errorf("at least one --share is required (form: TAG=PATH[:ro])")
			}
			parsed, err := parseShares(shares)
			if err != nil {
				return err
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.RegisterMicroVM(context.Background(), &vzdv1.RegisterMicroVMRequest{
				Name:    args[0],
				BootIso: bootISO,
				Kernel:  kernel,
				Initrd:  initrd,
				Cmdline: cmdline,
				Shares:  parsed,
			})
			return err
		},
	}
	cmd.Flags().StringVar(&bootISO, "boot-iso", "", "absolute path to the ncl-init UKI ISO (read-only) — UKI mode")
	cmd.Flags().StringVar(&kernel, "kernel", "", "absolute path to a Linux kernel image — direct-Linux mode")
	cmd.Flags().StringVar(&initrd, "initrd", "", "absolute path to an initramfs (optional, direct-Linux mode)")
	cmd.Flags().StringVar(&cmdline, "cmdline", "", "kernel cmdline override (e.g. \"ncl.rootfs=virtiofs:rootfs0\")")
	cmd.Flags().StringArrayVar(&shares, "share", nil, "virtio-fs share spec, form TAG=PATH[:ro] (repeat for multiple)")
	return cmd
}

// parseShares turns CLI `--share TAG=PATH[:ro]` strings into proto
// MicroVMShare messages. Validates each entry has a non-empty tag
// and an absolute path.
func parseShares(raw []string) ([]*vzdv1.MicroVMShare, error) {
	out := make([]*vzdv1.MicroVMShare, 0, len(raw))
	for _, s := range raw {
		entry, err := parseOneShare(s)
		if err != nil {
			return nil, fmt.Errorf("--share %q: %w", s, err)
		}
		out = append(out, entry)
	}
	return out, nil
}

func parseOneShare(s string) (*vzdv1.MicroVMShare, error) {
	tag, rest, ok := strings.Cut(s, "=")
	if !ok || tag == "" {
		return nil, fmt.Errorf("missing TAG= prefix")
	}
	path := rest
	ro := false
	if strings.HasSuffix(rest, ":ro") {
		path = strings.TrimSuffix(rest, ":ro")
		ro = true
	}
	if path == "" {
		return nil, fmt.Errorf("missing PATH")
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("PATH must be absolute (got %q)", path)
	}
	return &vzdv1.MicroVMShare{
		Tag:      tag,
		Path:     path,
		ReadOnly: ro,
	}, nil
}
