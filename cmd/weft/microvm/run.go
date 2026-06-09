// run.go implements `weft microvm run` — boots a microVM from an OCI
// image, the Docker-`run` analogue. Host-side prep + RegisterMicroVM
// + StartVM all live in the shared weft-microvm library; this file is
// the thin cobra front-end that builds microvm.Args from the flags.
package microvm

import (
	"fmt"
	"strings"

	"github.com/openweft/weft-microvm"
	"github.com/spf13/cobra"
)

// runCmd returns the `weft microvm run` command. The WeftSocket field
// is sourced from the shared --socket flag so the library dials the
// same agent the rest of the CLI talks to.
func runCmd(socket *string) *cobra.Command {
	var (
		mountTag     string
		detach       bool
		project      string
		pod          string
		mounts       []string
		cubefsMounts []string
	)
	cmd := &cobra.Command{
		Use:   "run IMAGE[:TAG] [-- CMD...]",
		Short: "Boot a microVM from an OCI image",
		Long: `Boots a microVM from an OCI image (auto-pulls on cache miss).

Everything after a "--" separator overrides the image's
entrypoint+cmd, e.g.:

	weft microvm run alpine:3.21 -- sh -c "echo hi"

Hostpath bind mounts use the Docker -v syntax:

	weft microvm run \
	  -v /host/project:/workspace \
	  -v /host/scratch:/workspace/.build \
	  ghcr.io/openweft/weft-loom-texlive \
	  -- latexmk -pdf -outdir=/workspace/.build /workspace/main.tex

Each --mount becomes a virtio-fs share inside the guest, mounted at
the requested path before the container starts. Append ":ro" to mount
read-only.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Split the positional args at the cobra "--" boundary.
			image := args[0]
			var cmdOverride []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				cmdOverride = args[dash:]
			}
			parsedMounts, err := parseMounts(mounts)
			if err != nil {
				return err
			}
			parsedCubeFS, err := parseCubeFSMounts(cubefsMounts)
			if err != nil {
				return err
			}
			return microvm.Run(microvm.Args{
				Image:        image,
				Cmd:          cmdOverride,
				Detach:       detach,
				MountTag:     mountTag,
				WeftSocket:   *socket,
				Project:      project,
				Pod:          pod,
				Mounts:       parsedMounts,
				CubeFSMounts: parsedCubeFS,
			})
		},
	}
	cmd.Flags().StringVar(&mountTag, "mount-tag", "", "virtio-fs tag exposed inside the guest (default rootfs0)")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "return once the VM is alive instead of streaming stdio")
	cmd.Flags().StringVar(&project, "project", "", "project namespace (empty = agent default)")
	cmd.Flags().StringVar(&pod, "pod", "", "path to a pod manifest JSON (multi-container mode)")
	cmd.Flags().StringArrayVarP(&mounts, "mount", "v", nil, "hostpath bind mount, HOST:GUEST[:ro] (Docker -v style ; repeat for multiple)")
	cmd.Flags().StringArrayVar(&cubefsMounts, "cubefs", nil, "CubeFS mount, MASTER[,MASTER...]:VOLUME:GUEST[:ro] (cfs-client in the initramfs mounts FUSE inside the guest ; repeat for multiple)")
	return cmd
}

// parseCubeFSMounts splits each --cubefs string
// ("MASTERS:VOLUME:GUEST" or "MASTERS:VOLUME:GUEST:ro") into typed
// microvm.CubeFSMount entries. MASTERS is comma-separated.
func parseCubeFSMounts(specs []string) ([]microvm.CubeFSMount, error) {
	out := make([]microvm.CubeFSMount, 0, len(specs))
	for _, s := range specs {
		parts := strings.Split(s, ":")
		if len(parts) < 3 {
			return nil, fmt.Errorf("--cubefs %q : expected MASTERS:VOLUME:GUEST[:ro], got %d colon-separated parts", s, len(parts))
		}
		m := microvm.CubeFSMount{
			Masters:   strings.Split(parts[0], ","),
			Volume:    parts[1],
			GuestPath: parts[2],
		}
		if len(parts) >= 4 {
			switch parts[3] {
			case "ro":
				m.ReadOnly = true
			case "rw":
				m.ReadOnly = false
			default:
				return nil, fmt.Errorf("--cubefs %q : unrecognised modifier %q (expected ro or rw)", s, parts[3])
			}
		}
		if m.Volume == "" || m.GuestPath == "" || len(m.Masters) == 0 || m.Masters[0] == "" {
			return nil, fmt.Errorf("--cubefs %q : MASTERS, VOLUME and GUEST must all be non-empty", s)
		}
		out = append(out, m)
	}
	return out, nil
}

// parseMounts splits the --mount strings ("HOST:GUEST" or
// "HOST:GUEST:ro") into typed microvm.Mount entries. The HOST + GUEST
// halves are both required ; ":ro" is the only trailing-segment modifier
// supported today.
func parseMounts(specs []string) ([]microvm.Mount, error) {
	out := make([]microvm.Mount, 0, len(specs))
	for _, s := range specs {
		parts := strings.Split(s, ":")
		if len(parts) < 2 {
			return nil, fmt.Errorf("--mount %q : expected HOST:GUEST[:ro], got %d colon-separated parts", s, len(parts))
		}
		m := microvm.Mount{HostPath: parts[0], GuestPath: parts[1]}
		// Trailing modifier — only "ro" recognised today. A future
		// "rw" (the default) + "shared" / "private" propagation
		// would slot in here.
		if len(parts) >= 3 {
			switch parts[2] {
			case "ro":
				m.ReadOnly = true
			case "rw":
				m.ReadOnly = false
			default:
				return nil, fmt.Errorf("--mount %q : unrecognised modifier %q (expected ro or rw)", s, parts[2])
			}
		}
		if m.HostPath == "" || m.GuestPath == "" {
			return nil, fmt.Errorf("--mount %q : HOST and GUEST paths cannot be empty", s)
		}
		out = append(out, m)
	}
	return out, nil
}
