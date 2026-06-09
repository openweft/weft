package main

// infra.go wires `weft infra deploy <service>` into weft's cobra
// command tree. The subcommand reads a plan from
// pkg/openweft/weft/infra/<service>/plan.hcl (overridable via
// --plan), validates the pre-pulled OCI rootfs is on disk, then
// calls Adapter.RegisterMicroVM + Adapter.StartVM in-process —
// same code path `weft-microvm run` uses over gRPC.
//
// In-process rather than gRPC because `weft infra deploy` is a
// bootstrap command: it runs before the daemon's network surface
// is even reachable for some early services (etcd, dex). The
// Adapter API is the right boundary.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/openweft/weft"
	"github.com/openweft/weft/infra"
)

func newInfraCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "infra",
		Short: "Manage cloud-platform infrastructure services (etcd, dex, zot, …)",
	}
	cmd.AddCommand(newInfraDeployCmd())
	cmd.AddCommand(newInfraBootstrapCmd())
	cmd.AddCommand(newInfraStatusCmd())
	cmd.AddCommand(newInfraValidateCmd())
	return cmd
}

func newInfraDeployCmd() *cobra.Command {
	var planPath string
	var rootfsPath string
	var stateDir string
	var waitHealth bool
	var healthTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "deploy <service>",
		Short: "Deploy an infrastructure service from its HCL plan",
		Long: `Deploy reads pkg/openweft/weft/infra/<service>/plan.hcl, validates
the pre-pulled OCI rootfs is on disk (operator runs weft-microvm pull first),
and registers + starts a micro-VM for the service. The VM lives in
the "infra" project and is named "infra-<service>".

With --wait-health the deployer polls the plan's health block
URL (type=http, cmd=URL) until it returns 2xx (or --health-timeout
elapses). Use the literal token $VM_IP in the health URL ; the
deployer substitutes it with the booted VM's IP at probe time.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			service := args[0]
			path := planPath
			if path == "" {
				path = infra.DefaultPlanPath(moduleRoot(), service)
			}
			p, err := infra.LoadPlan(path)
			if err != nil {
				return err
			}
			if p.Service != service {
				return fmt.Errorf("plan label %q does not match argument %q (plan at %s)", p.Service, service, path)
			}
			a := weft.New(stateDir)
			return deployPlan(a, p, rootfsPath, stateDir, waitHealth, healthTimeout)
		},
	}
	cmd.Flags().StringVar(&planPath, "plan", "", "Path to plan.hcl (default: pkg/openweft/weft/infra/<service>/plan.hcl)")
	cmd.Flags().StringVar(&rootfsPath, "rootfs", "", "Override the OCI rootfs path (default: weft-microvm image-cache layout)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "state", "weft state directory (where the VM lands on disk)")
	cmd.Flags().BoolVar(&waitHealth, "wait-health", false, "After StartVM, poll the plan's health URL until it returns 2xx")
	cmd.Flags().DurationVar(&healthTimeout, "health-timeout", 60*time.Second, "Total time to wait for a service to become healthy (only consulted with --wait-health)")
	return cmd
}

// newInfraBootstrapCmd brings up multiple infrastructure services
// in dependency order. Without `--services`, it discovers every
// plan under infra/ and topologically sorts them. The Adapter is
// shared across all deploys so registry mutations (project,
// volumes, …) stay consistent across services.
func newInfraBootstrapCmd() *cobra.Command {
	var stateDir string
	var serviceFilter []string
	var waitHealth bool
	var healthTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "bootstrap [--services name,name,...]",
		Short: "Deploy multiple infra services in dependency order",
		Long: `Bootstrap loads every plan.hcl under infra/, sorts them by
depends_on (a → b means a is deployed before b), and deploys each
one in order using a shared weft Adapter.

Without --services the whole infra tree gets deployed in topo
order. With --services, only the named services are loaded; their
depends_on must also resolve inside the filtered set (otherwise
the bootstrap errors so the operator sees the gap clearly).

With --wait-health the deployer gates each service on its
health probe before moving to the next : a dependent only
starts once its prerequisite is responding 2xx. Use the literal
$VM_IP token in the plan's health.cmd ; it's substituted at
poll time.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			root := moduleRoot()
			services, err := resolveBootstrapServices(root, serviceFilter)
			if err != nil {
				return err
			}
			plans, err := infra.LoadAllPlans(root, services)
			if err != nil {
				return err
			}
			ordered, err := infra.TopologicalSort(plans)
			if err != nil {
				return err
			}
			a := weft.New(stateDir)
			for _, p := range ordered {
				logger.Printf("infra bootstrap: deploying %s", p.Service)
				if err := deployPlan(a, p, "", stateDir, waitHealth, healthTimeout); err != nil {
					return fmt.Errorf("deploy %s: %w", p.Service, err)
				}
			}
			logger.Printf("infra bootstrap: %d services up", len(ordered))
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "state", "weft state directory")
	cmd.Flags().StringSliceVar(&serviceFilter, "services", nil, "Comma-separated subset of services to deploy (default: all under infra/)")
	cmd.Flags().BoolVar(&waitHealth, "wait-health", false, "After StartVM, poll the plan's health URL until 2xx before moving to the next service")
	cmd.Flags().DurationVar(&healthTimeout, "health-timeout", 60*time.Second, "Total time to wait for a service to become healthy (only consulted with --wait-health)")
	return cmd
}

// resolveBootstrapServices returns the list of service names to
// deploy: the operator-supplied filter when non-empty (validated
// against the available plans), otherwise every plan on disk.
func resolveBootstrapServices(moduleRoot string, filter []string) ([]string, error) {
	available, err := infra.ListServices(moduleRoot)
	if err != nil {
		return nil, err
	}
	if len(filter) == 0 {
		return available, nil
	}
	availSet := make(map[string]struct{}, len(available))
	for _, s := range available {
		availSet[s] = struct{}{}
	}
	var missing []string
	for _, s := range filter {
		if _, ok := availSet[s]; !ok {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("infra bootstrap: no plan for service(s): %s", strings.Join(missing, ", "))
	}
	return filter, nil
}

// newInfraStatusCmd reports which infra services have a VM
// registered + whether it's running. Read-only — useful right
// after `weft infra bootstrap` to verify the topo-ordered deploy
// landed everything. Joins on `infra-<service>` so the output
// stays stable across project renames.
func newInfraStatusCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the deploy state of every infra service",
		Long: `Status walks the plans under infra/ and reports the VM state for
each one. Output is tab-separated for easy grep / awk piping:

  SERVICE  STATE     PROJECT  IMAGE

State is one of:
  not-registered   no VM dir exists yet (operator hasn't run deploy / bootstrap)
  stopped          VM dir exists; vm.pid is absent or stale
  running          vm.pid present and the process is alive`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			root := moduleRoot()
			services, err := infra.ListServices(root)
			if err != nil {
				return err
			}
			plans, err := infra.LoadAllPlans(root, services)
			if err != nil {
				return err
			}
			a := weft.New(stateDir)
			vms, err := a.ListLocal()
			if err != nil {
				return fmt.Errorf("list local vms: %w", err)
			}
			fmt.Printf("SERVICE\tSTATE\tPROJECT\tIMAGE\n")
			for _, s := range services {
				p := plans[s]
				for replica := 1; replica <= p.ReplicaCount(); replica++ {
					vmName := p.VMNameFor(replica)
					state := "not-registered"
					project := p.Project
					if entry, ok := vms[vmName]; ok {
						if v, _ := entry["State"].(string); v != "" {
							state = v
						}
						if v, _ := entry["project"].(string); v != "" {
							project = v
						}
					}
					// Use the VM name (not just the plan name) on
					// the first column so multi-replica services
					// are unambiguous; for count=1 this is the
					// legacy `infra-<service>` shape, matching the
					// VM the operator would `weft start/stop`.
					fmt.Printf("%s\t%s\t%s\t%s\n", vmName, state, project, p.OCIImage)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "state", "weft state directory")
	return cmd
}

// newInfraValidateCmd is a dry-run lint of the plan tree: load
// every plan, run the same checks LoadAllPlans + TopologicalSort
// do, print the resolved deploy order. No VM is registered, no
// state is mutated — operators run it before `bootstrap` to
// catch typos (unknown attributes, missing depends_on, plan
// label vs directory mismatch, dependency cycles) without having
// to half-deploy and roll back.
func newInfraValidateCmd() *cobra.Command {
	var serviceFilter []string
	cmd := &cobra.Command{
		Use:   "validate [--services name,name,...]",
		Short: "Dry-run check of every plan: parse, topo-sort, print order",
		Long: `Validate loads each plan under infra/, runs the same
parse / dependency / cycle checks bootstrap would run, and
prints the deploy order it would use. Read-only — nothing is
registered or started.

Use it after editing a plan file to catch shape mistakes before
bootstrap: unknown attributes (HCL strict mode), missing
depends_on targets, dependency cycles, plan label vs directory
mismatches. Exits non-zero on the first error so a CI gate can
fail-fast.

With --services, only the listed plans are loaded.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			root := moduleRoot()
			services, err := resolveBootstrapServices(root, serviceFilter)
			if err != nil {
				return err
			}
			plans, err := infra.LoadAllPlans(root, services)
			if err != nil {
				return err
			}
			ordered, err := infra.TopologicalSort(plans)
			if err != nil {
				return err
			}
			fmt.Printf("# %d plan(s) validated, deploy order:\n", len(ordered))
			for i, p := range ordered {
				deps := "—"
				if len(p.DependsOn) > 0 {
					deps = strings.Join(p.DependsOn, ",")
				}
				fmt.Printf("%2d. %s\t(depends_on: %s)\n", i+1, p.Service, deps)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&serviceFilter, "services", nil, "Comma-separated subset of services to validate (default: all under infra/)")
	return cmd
}

// deployPlan registers + starts one service's micro-VM via the
// given Adapter. Shared between `infra deploy` and `infra
// bootstrap` so the two stay consistent on rootfs validation,
// boot artefact paths, and the log lines they emit. `stateDir`
// is the weft state root — used to anchor the host-side scratch
// directory for materialised config files.
func deployPlan(a weft.VZAdapter, p *infra.Plan, rootfsOverride, stateDir string, waitHealth bool, healthTimeout time.Duration) error {
	rootfs := rootfsOverride
	if rootfs == "" {
		rootfs = p.DefaultRootfsPath()
	}
	if _, err := os.Stat(rootfs); err != nil {
		return fmt.Errorf("rootfs %q not found — run `weft-microvm pull %s` first: %w", rootfs, p.OCIImage, err)
	}
	replicas := p.ReplicaCount()

	// Scheduler integration : when at least one Host is
	// registered, ask the scheduler to pick one Host per replica
	// honouring the plan's placement rule. The picked Host[i].AZ
	// then drives `$DC` for replica i (more accurate than the
	// synthetic `dc<i>` label). With zero hosts the loop falls
	// through to the legacy single-host all-in-one path — that's
	// the Mac-laptop default where there's nothing to schedule
	// against.
	var pickedHosts []weft.Host
	if len(a.Hosts()) > 0 {
		req, err := p.GroupScheduleRequest(p.Project, "")
		if err != nil {
			return fmt.Errorf("build schedule request for %s: %w", p.Service, err)
		}
		pickedHosts, err = a.ScheduleVMGroup(context.Background(), req)
		if err != nil {
			return fmt.Errorf("schedule %s: %w", p.Service, err)
		}
		logger.Printf("infra deploy: scheduled %s onto %d host(s)", p.Service, len(pickedHosts))
	}

	for replica := 1; replica <= replicas; replica++ {
		var az string
		if i := replica - 1; i < len(pickedHosts) {
			az = pickedHosts[i].AZ
		}
		if err := deployReplica(a, p, rootfs, stateDir, replica, az, waitHealth, healthTimeout); err != nil {
			return err
		}
	}
	return nil
}

// deployReplica registers + starts one replica of a plan. Called
// once for single-replica plans, N times for multi-replica
// (placement.count = N). Each replica gets its own VM name +
// rendered config-file scratch dir so they don't clash.
//
// `pickedAZ` is the AZ name of the Host the scheduler picked for
// this replica (or "" when there are no Hosts registered and
// the deploy falls back to local-only mode). When set, it
// replaces the synthetic `dc<i>` label in the rendered config —
// see [[weft-placement-rules]] + BuildReplicaContextWithHost.
//
// RegisterMicroVM still always lands on the local host today ;
// when weft-agent's per-host gRPC ControlPlane is wired, that
// call gates on `pickedHost.UUID` to dispatch to the right node.
func deployReplica(a weft.VZAdapter, p *infra.Plan, rootfs, stateDir string, replica int, pickedAZ string, waitHealth bool, healthTimeout time.Duration) error {
	vmName := p.VMNameFor(replica)
	boot := weft.MicroVMBoot{
		Kernel:  infra.DefaultArtefact("kernel"),
		Initrd:  infra.DefaultArtefact("initrd"),
		Cmdline: p.CmdlineForGuest(),
	}
	shares := []weft.MicroVMShare{
		{Tag: "rootfs0", Path: rootfs, ReadOnly: false, Clone: true},
	}
	// If the plan declares a config_file block, materialise the
	// per-replica template under <stateDir>/infra-config/<service>
	// (or `<service>-dc<N>` for multi-replica) and expose it as a
	// read-only virtio-fs share at tag "cfg". Token substitution
	// uses BuildReplicaContextWithHost(p, replica, pickedAZ) so
	// $REPLICA / $DC / $PRIVATE_IP / $PEERS / $PEER_DC reflect
	// the per-replica placement, with $DC sourced from the picked
	// host's AZ when one is available.
	if p.ConfigFile != nil {
		scratch := filepath.Join(stateDir, "infra-config")
		ctx := infra.BuildReplicaContextWithHost(p, replica, pickedAZ)
		cfgDir, err := infra.MaterialiseConfigFile(p, scratch, ctx)
		if err != nil {
			return fmt.Errorf("materialise config_file for %s: %w", vmName, err)
		}
		if cfgDir != "" {
			shares = append(shares, weft.MicroVMShare{
				Tag:      "cfg",
				Path:     cfgDir,
				ReadOnly: true,
			})
		}
	}
	if err := a.RegisterMicroVM(p.Project, vmName, boot, shares); err != nil {
		return fmt.Errorf("register %s: %w", vmName, err)
	}
	logger.Printf("infra deploy: registered %s (image=%s project=%s cpu=%d mem=%dMiB replica=%d/%d)",
		vmName, p.OCIImage, p.Project, p.CPU(), p.MemoryMiB(), replica, p.ReplicaCount())
	if err := a.StartVM(vmName, ""); err != nil {
		return fmt.Errorf("start %s: %w", vmName, err)
	}
	logger.Printf("infra deploy: started %s", vmName)
	fmt.Printf("%s\t%s\t%s\n", vmName, p.Project, p.OCIImage)
	if waitHealth && p.Health != nil {
		if err := awaitHealthy(a, vmName, p, healthTimeout); err != nil {
			return fmt.Errorf("health %s: %w", vmName, err)
		}
		logger.Printf("infra deploy: %s healthy", vmName)
	}
	return nil
}

// awaitHealthy ties together the two waits the operator needs
// after StartVM : first the VM picks up its host-side IP, then
// the plan's health URL goes 2xx. Either step is bounded by
// `healthTimeout` (split 50/50 — the operator passes one total
// budget, the deployer divides). The IP discovery uses
// Adapter.IP polling ; the HTTP probe uses infra.WaitHealthy.
func awaitHealthy(a weft.VZAdapter, vmName string, p *infra.Plan, totalTimeout time.Duration) error {
	if totalTimeout <= 0 {
		totalTimeout = 60 * time.Second
	}
	half := totalTimeout / 2
	period := infra.HealthPeriod(p)

	// Phase 1 : wait for VM IP. The Adapter populates this once
	// vmnet hands out a lease — typically <5s on Apple-VZ. The
	// loop deadlines independently of the HTTP probe so a slow
	// boot doesn't eat the whole budget.
	ipDeadline := time.Now().Add(half)
	var vmIP string
	for {
		ip, err := a.IP(vmName)
		if err == nil && ip != "" {
			vmIP = ip
			break
		}
		if time.Now().After(ipDeadline) {
			return fmt.Errorf("waited %s for IP, last err: %v", half, err)
		}
		time.Sleep(period)
	}
	logger.Printf("infra deploy: %s reachable at %s", vmName, vmIP)

	// Phase 2 : substitute $VM_IP into the plan's health URL
	// and poll until 2xx.
	url, err := infra.HealthURL(p, vmIP)
	if err != nil {
		return fmt.Errorf("build health URL: %w", err)
	}
	logger.Printf("infra deploy: probing %s every %s up to %s", url, period, half)
	return infra.WaitHealthy(context.Background(), url, half, period)
}

// moduleRoot resolves the absolute path of the weft module so
// DefaultPlanPath can find plan.hcl regardless of where the
// operator runs `weft infra deploy` from. Uses runtime caller
// info — the package itself lives at <module-root>/cmd/weft/.
func moduleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		// Fallback: assume CWD. The DefaultPlanPath stat will
		// fail with a clear message anyway.
		return "."
	}
	// file = <module-root>/cmd/weft/infra.go → go up two dirs.
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
