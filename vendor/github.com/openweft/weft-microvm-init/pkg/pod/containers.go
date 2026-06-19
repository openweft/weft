package pod

// containers.go — the wire shape for the per-VM "containers" subject
// (`weft.containers.<vmID>`). The reconciler in
// weft-microvm-agent/pkg/containers consumes ContainerSet whole and
// drives crun to converge the running set : pull missing images,
// start containers not yet running, stop containers no longer in
// the set. Replace-by-name semantics make a missed message
// self-heal on the next publish — same model pkg/mounts uses for
// shares.
//
// Symmetric with ShareMount on the wire :
//   - boot-time : Spec.Containers may pre-populate the VM with
//     a fixed workload (legacy fixed-image microVMs)
//   - dynamic   : ContainerSet is the message an operator (loom-
//     server, weft-agent reconciler, an `weft microvm exec`
//     command) publishes to spawn / kill on a running VM
//
// Image resolution mirrors how the existing microVM image pull works
// in pkg/boot : OCI reference, ncl-pulled into the VM's local image
// store, then crun runs it from a bundle prepared by ncl unpack.

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ContainerSetVersion lets the reconciler reject payloads it doesn't
// know how to apply. Bumped when the schema changes incompatibly.
const ContainerSetVersion = 1

// ContainerSet is the JSON payload the loom-server / control plane
// publishes whole on `weft.containers.<vmID>`. The agent diffs it
// against the running set and converges.
type ContainerSet struct {
	Version    int         `json:"version"`
	Containers []WorkloadContainer `json:"containers"`
}

// WorkloadContainer describes one container the agent should keep running.
// Name is the stable identity for the diff ; image is the OCI ref
// to pull. Command/Args override the image's entrypoint. Env +
// Mounts + WorkingDir + Net give the workload its execution
// environment. Restart governs what the agent does when the
// container exits.
type WorkloadContainer struct {
	// Name is the stable handle the diff uses. Required, unique
	// within a ContainerSet. The reconciler maps this to crun's
	// container ID + the workload's process group.
	Name string `json:"name"`

	// Image is the OCI reference (e.g. "ghcr.io/openweft/weft-loom-texlive:v0.1.0").
	// Pulled via ncl into the VM's local image store on first use
	// (idempotent — second publish of the same image is a no-op
	// once the manifest digest matches).
	Image string `json:"image"`

	// Command overrides the image's entrypoint. Empty = use image
	// default. The shell-tool-wrapper pattern (a tiny script in
	// /usr/local/bin that delegates to crun exec) keeps Command
	// empty here and uses Restart="never" with a sleep entrypoint
	// so the container is a long-running shell-able sandbox.
	Command []string `json:"command,omitempty"`

	// Args appended to Command. Same semantics as Kubernetes args.
	Args []string `json:"args,omitempty"`

	// Env is K=V pairs merged onto the image's default env. The agent
	// passes them via crun's `--env` flag on start.
	Env map[string]string `json:"env,omitempty"`

	// Mounts pulls host paths into the container. The most common
	// entry is the per-user `/workspace` mount that already lives
	// on CubeFS — the host path is the ShareMount's MountPoint.
	Mounts []ContainerMount `json:"mounts,omitempty"`

	// WorkingDir sets the container's cwd. Defaults to "/workspace".
	WorkingDir string `json:"working_dir,omitempty"`

	// Net selects how the container reaches the network :
	//   "host"   — share the VM's network namespace (default for
	//              build tools : they need internet for go mod / pip)
	//   "none"   — no network (offline-only tools)
	//   "shared" — share another container's namespace (sidecars)
	Net string `json:"net,omitempty"`

	// Restart governs the supervisor loop :
	//   "always"   — restart on exit (the default for shell-able
	//                sandboxes : the container stays warm so each
	//                `crun exec` lands instantly)
	//   "on-failure" — only restart if exit code != 0
	//   "never"      — fire-and-forget (one-shot compile jobs)
	Restart string `json:"restart,omitempty"`

	// Annotations carries opaque metadata the loom-doctor surface
	// can use to thread events back to a user request. Not consumed
	// by the runtime.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ContainerMount maps a host path into the container. The host path
// is typically a ShareMount.MountPoint already on CubeFS, so the
// container's writes propagate to peers via CubeFS without an extra
// layer.
type ContainerMount struct {
	// HostPath is the source on the guest's filesystem.
	HostPath string `json:"host_path"`
	// MountPath is where the container sees it.
	MountPath string `json:"mount_path"`
	// Readonly mounts the directory read-only inside the container.
	Readonly bool `json:"readonly,omitempty"`
}

// Validate runs the per-field sanity checks the reconciler should
// not have to repeat. Returns nil iff the set is internally
// consistent and the agent can attempt to apply it ; an error here
// means "publisher bug, drop the message + alert" (loom-doctor
// surfaces it).
func (s *ContainerSet) Validate() error {
	if s == nil {
		return errors.New("nil ContainerSet")
	}
	if s.Version == 0 {
		// Tolerate a missing version on the wire as v1 (current).
		s.Version = ContainerSetVersion
	}
	if s.Version > ContainerSetVersion {
		return fmt.Errorf("unsupported ContainerSet version %d (agent supports %d)", s.Version, ContainerSetVersion)
	}
	seen := map[string]struct{}{}
	for i := range s.Containers {
		c := &s.Containers[i]
		if c.Name == "" {
			return fmt.Errorf("containers[%d]: name required", i)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("containers[%d]: duplicate name %q", i, c.Name)
		}
		seen[c.Name] = struct{}{}
		if c.Image == "" {
			return fmt.Errorf("containers[%d]: image required (got name=%q)", i, c.Name)
		}
		switch c.Net {
		case "", "host", "none", "shared":
		default:
			return fmt.Errorf("containers[%d]: invalid net %q (host|none|shared|empty)", i, c.Net)
		}
		switch c.Restart {
		case "", "always", "on-failure", "never":
		default:
			return fmt.Errorf("containers[%d]: invalid restart %q (always|on-failure|never|empty)", i, c.Restart)
		}
		for j, m := range c.Mounts {
			if m.HostPath == "" || m.MountPath == "" {
				return fmt.Errorf("containers[%d].mounts[%d]: host_path + mount_path required", i, j)
			}
		}
	}
	return nil
}

// MarshalSet is a stable encoder so the agent + the publisher both
// generate the same bytes — useful for stream-replay debugging.
func (s *ContainerSet) MarshalSet() ([]byte, error) {
	if s.Version == 0 {
		s.Version = ContainerSetVersion
	}
	return json.Marshal(s)
}
