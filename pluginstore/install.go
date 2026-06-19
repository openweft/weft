package pluginstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	weftv1 "github.com/openweft/weft-proto"
)

// Client is the subset of agent RPCs the orchestrator needs. The
// methods are deliberately simpler than weftv1.WeftAgentClient's
// (no variadic grpc.CallOption) — package agentclient (adjacent
// file) supplies a thin adapter that wraps the real gRPC stub.
// Keeping the interface this narrow makes fake clients trivial.
type Client interface {
	CreateNetwork(ctx context.Context, in *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error)
	CreateSecurityGroup(ctx context.Context, in *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error)
	SetNetworkDefaultSecurityGroups(ctx context.Context, in *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error)
	CreateVM(ctx context.Context, in *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error)
	DeleteVM(ctx context.Context, in *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error)
	DeleteNetwork(ctx context.Context, in *weftv1.DeleteNetworkRequest) (*weftv1.DeleteNetworkResponse, error)
	DeleteSecurityGroup(ctx context.Context, in *weftv1.DeleteSecurityGroupRequest) (*weftv1.DeleteSecurityGroupResponse, error)
	CreateVolume(ctx context.Context, in *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error)
	DeleteVolume(ctx context.Context, in *weftv1.DeleteVolumeRequest) (*weftv1.DeleteVolumeResponse, error)
	// MicroVMRun drives the host-side weft-microvm orchestration
	// (auto-pull → RegisterMicroVM → StartVM) for the V0.5
	// runtime="microvm" plugin path.
	MicroVMRun(ctx context.Context, image, project string) error
	SetVMProperties(ctx context.Context, in *weftv1.SetVMPropertiesRequest) (*weftv1.SetVMPropertiesResponse, error)
}

// Manager orchestrates Install / Uninstall / List.
type Manager struct {
	client Client
	state  StateStore
	// now is overridable so tests get a deterministic timestamp.
	now func() time.Time
}

// NewManager builds a Manager over the given client and state store.
func NewManager(c Client, s StateStore) *Manager {
	return &Manager{client: c, state: s, now: time.Now}
}

// microvmRefsafe mirrors the naming weft-microvm.Run derives from an
// OCI image ref : prefix "weft-microvm-" + the ref with '/' and ':'
// replaced by '_'. Kept as a helper here so the plugin installer can
// stamp properties by name after microvm.Run completes, without exposing
// the orchestration's internal naming scheme as part of its public
// surface.
func microvmRefsafe(image string) string {
	out := make([]byte, 0, len("weft-microvm-")+len(image))
	out = append(out, "weft-microvm-"...)
	for i := 0; i < len(image); i++ {
		c := image[i]
		if c == '/' || c == ':' {
			c = '_'
		}
		out = append(out, c)
	}
	return string(out)
}

// Install applies the manifest. Idempotency : two calls with the
// same (project, name, inputs) hash resolve to the same instance
// UUID and short-circuit before issuing a single CreateVM.
//
// The order is fixed : networks → security groups → SG-to-network
// binding → volumes → VMs. Tear-down (Uninstall) goes in reverse.
func (m *Manager) Install(ctx context.Context, manifest *Manifest, project string, inputs map[string]any) (Instance, error) {
	if m.client == nil {
		return Instance{}, errors.New("plugin install: nil agent client")
	}
	resolved, err := manifest.ValidateInputs(inputs)
	if err != nil {
		return Instance{}, err
	}
	uuid := DeterministicInstanceUUID(manifest.Name, project, resolved)

	// Idempotent re-run : if a record already exists for this
	// (name, uuid) we return it as-is. The caller can tell from
	// InstalledAt being older than the wallclock that nothing
	// new happened. Re-applying a CHANGED set of inputs creates
	// a different UUID, so this isn't a "drift fixup" path —
	// drift is reconciled by weft-network independently.
	if existing, ok, err := m.state.Get(manifest.Name, uuid); err != nil {
		return Instance{}, fmt.Errorf("plugin install: state lookup: %w", err)
	} else if ok {
		return existing, nil
	}

	inst := Instance{
		Name:        manifest.Name,
		UUID:        uuid,
		Version:     manifest.Version,
		Project:     project,
		InstalledAt: m.now(),
		Inputs:      maskSecrets(manifest, resolved),
	}

	// ---- Networks -------------------------------------------------
	netUUIDByName := map[string]string{}
	for _, spec := range manifest.Networks {
		nt := spec.Type
		if nt == "" {
			nt = "nat"
		}
		resp, err := m.client.CreateNetwork(ctx, &weftv1.CreateNetworkRequest{
			Project:    project,
			Name:       qualifiedName(manifest.Name, uuid, spec.Name),
			Cidr:       spec.CIDR,
			Gateway:    spec.Gateway,
			DnsServers: spec.DNS,
			Type:       nt,
		})
		if err != nil {
			return inst, m.rollback(ctx, inst, fmt.Errorf("create network %q: %w", spec.Name, err))
		}
		if resp.Network != nil {
			netUUIDByName[spec.Name] = resp.Network.Uuid
			inst.Networks = append(inst.Networks, resp.Network.Uuid)
		}
	}

	// ---- Security groups -----------------------------------------
	sgUUIDByName := map[string]string{}
	for _, sg := range manifest.SecurityGroups {
		rules := make([]*weftv1.SecurityRule, 0, len(sg.Rules))
		for _, r := range sg.Rules {
			proto := r.Protocol
			if proto == "" {
				proto = "any"
			}
			rules = append(rules, &weftv1.SecurityRule{
				Direction:  r.Direction,
				Protocol:   proto,
				PortMin:    int32(r.PortMin),
				PortMax:    int32(r.PortMax),
				RemoteCidr: r.RemoteCIDR,
			})
		}
		resp, err := m.client.CreateSecurityGroup(ctx, &weftv1.CreateSecurityGroupRequest{
			Project:     project,
			Name:        qualifiedName(manifest.Name, uuid, sg.Name),
			Description: sg.Description,
			Rules:       rules,
		})
		if err != nil {
			return inst, m.rollback(ctx, inst, fmt.Errorf("create security group %q: %w", sg.Name, err))
		}
		if resp.Group != nil {
			sgUUIDByName[sg.Name] = resp.Group.Uuid
			inst.SecurityGroups = append(inst.SecurityGroups, resp.Group.Uuid)
		}
	}
	// Network ↔ SG binding. We aggregate all SGs whose `networks`
	// list mentions a given network and apply them atomically via
	// SetNetworkDefaultSecurityGroups — matches the registry's
	// "replace whole list" semantics.
	for netName, netUUID := range netUUIDByName {
		var bound []string
		for _, sg := range manifest.SecurityGroups {
			if !contains(sg.Networks, netName) {
				continue
			}
			if uuid, ok := sgUUIDByName[sg.Name]; ok {
				bound = append(bound, uuid)
			}
		}
		if len(bound) == 0 {
			continue
		}
		if _, err := m.client.SetNetworkDefaultSecurityGroups(ctx, &weftv1.SetNetworkDefaultSecurityGroupsRequest{
			Uuid:               netUUID,
			SecurityGroupUuids: bound,
		}); err != nil {
			return inst, m.rollback(ctx, inst, fmt.Errorf("bind sgs to network %q: %w", netName, err))
		}
	}

	// ---- VMs (with optional per-replica volumes) -----------------
	for _, vm := range manifest.VMs {
		schedRule := vm.SchedulingRule
		if schedRule == "" {
			schedRule = m.deriveSchedulingRule(manifest, vm, uuid)
		}
		netUUID := netUUIDByName[vm.Network]
		// Resolve the optional vm count attribute. count=1 (the
		// default) keeps the original naming — no "-0" suffix —
		// so existing manifests are byte-for-byte compatible.
		vmCount, err := resolveCount(vm.Count, resolved)
		if err != nil {
			return inst, m.rollback(ctx, inst, fmt.Errorf("vm %q: %w", vm.Name, err))
		}
		for c := 0; c < vmCount; c++ {
			baseName := vm.Name
			if vmCount > 1 {
				baseName = fmt.Sprintf("%s-%d", vm.Name, c)
			}
			for i := 0; i < vm.Replicas; i++ {
				vmName := replicaName(manifest.Name, uuid, baseName, i)
				// Allocate per-replica volumes before
				// CreateVM so the agent can attach at boot.
				// The attach step is left to the agent —
				// we just declare the volumes ; weft-network
				// reconciles the binding via its existing
				// watch loop.
				for _, vol := range vm.Volumes {
					volCount, err := resolveCount(vol.Count, resolved)
					if err != nil {
						return inst, m.rollback(ctx, inst, fmt.Errorf("vm %q volume %q: %w", vm.Name, vol.Name, err))
					}
					for vc := 0; vc < volCount; vc++ {
						volBase := vol.Name
						if volCount > 1 {
							volBase = fmt.Sprintf("%s-%d", vol.Name, vc)
						}
						volName := fmt.Sprintf("%s-%s", vmName, volBase)
						fmtStr := vol.Format
						if fmtStr == "" {
							fmtStr = "raw"
						}
						resp, err := m.client.CreateVolume(ctx, &weftv1.CreateVolumeRequest{
							Project: project,
							Name:    volName,
							SizeGib: int64(vol.SizeGiB),
							Format:  fmtStr,
						})
						if err != nil {
							return inst, m.rollback(ctx, inst, fmt.Errorf("create volume %q: %w", volName, err))
						}
						if resp.Volume != nil {
							inst.Volumes = append(inst.Volumes, resp.Volume.Uuid)
						}
					}
				}
				switch vm.Runtime {
				case "", "classic":
					req := &weftv1.CreateVMRequest{
						Project:        project,
						Name:           vmName,
						Image:          vm.Image,
						Cpu:            uint32(vm.CPU),
						MemMb:          uint64(vm.MemMB),
						DiskGb:         uint64(vm.DiskGB),
						SchedulingRule: schedRule,
						Network:        netUUID,
					}
					if _, err := m.client.CreateVM(ctx, req); err != nil {
						return inst, m.rollback(ctx, inst, fmt.Errorf("create vm %q: %w", vmName, err))
					}
				case "microvm":
					// V0.5 microvm runtime : the orchestration
					// (auto-pull → RegisterMicroVM → StartVM) lives
					// inside weft-microvm. We pass the OCI image and
					// project ; the registry name is derived inside
					// the library (refsafe form of the image).
					if err := m.client.MicroVMRun(ctx, vm.Image, project); err != nil {
						return inst, m.rollback(ctx, inst, fmt.Errorf("microvm run %q: %w", vmName, err))
					}
					// Apply plugin-declared properties (typically
					// deployment.type=ha + role=X) so the V0.1.10
					// respawn gate + V0.1.15 zombiegc see the
					// workload classification. SetVMProperties is
					// project-scoped + name-keyed.
					if len(vm.Properties) > 0 {
						properties := make(map[string]string, len(vm.Properties))
						for k, v := range vm.Properties {
							properties[k] = v
						}
						refsafeName := microvmRefsafe(vm.Image)
						if _, err := m.client.SetVMProperties(ctx, &weftv1.SetVMPropertiesRequest{
							Project:    project,
							Name:       refsafeName,
							Properties: properties,
						}); err != nil {
							return inst, m.rollback(ctx, inst, fmt.Errorf("set properties %q: %w", refsafeName, err))
						}
					}
				default:
					return inst, m.rollback(ctx, inst, fmt.Errorf("plugin install: unknown vm runtime %q (want \"\"|\"classic\"|\"microvm\")", vm.Runtime))
				}
				inst.VMs = append(inst.VMs, vmName)
			}
		}
	}

	if err := m.state.Put(inst); err != nil {
		// We've side-effected the cluster but can't record it.
		// Don't rollback — that's worse than a stale record. The
		// next install attempt will see no record and re-create,
		// which will collide on names and surface as a clear
		// error. The operator can manually `weft plugin
		// uninstall <name> --instance <uuid>` to clean up.
		return inst, fmt.Errorf("plugin install: persist instance: %w", err)
	}
	return inst, nil
}

// Uninstall tears down a plugin instance in reverse order. It tries
// every step even if individual deletes fail (best-effort), then
// removes the state record only when everything that mattered went
// through.
func (m *Manager) Uninstall(ctx context.Context, name, uuid string) error {
	inst, ok, err := m.state.Get(name, uuid)
	if err != nil {
		return fmt.Errorf("plugin uninstall: state lookup: %w", err)
	}
	if !ok {
		return fmt.Errorf("plugin uninstall: no instance %q/%s", name, uuid)
	}
	var errs []error
	for _, v := range inst.VMs {
		if _, err := m.client.DeleteVM(ctx, &weftv1.DeleteVMRequest{Name: v, Project: inst.Project}); err != nil {
			errs = append(errs, fmt.Errorf("delete vm %s: %w", v, err))
		}
	}
	for _, vol := range inst.Volumes {
		if _, err := m.client.DeleteVolume(ctx, &weftv1.DeleteVolumeRequest{Uuid: vol}); err != nil {
			errs = append(errs, fmt.Errorf("delete volume %s: %w", vol, err))
		}
	}
	for _, sg := range inst.SecurityGroups {
		if _, err := m.client.DeleteSecurityGroup(ctx, &weftv1.DeleteSecurityGroupRequest{Uuid: sg}); err != nil {
			errs = append(errs, fmt.Errorf("delete sg %s: %w", sg, err))
		}
	}
	for _, n := range inst.Networks {
		if _, err := m.client.DeleteNetwork(ctx, &weftv1.DeleteNetworkRequest{Uuid: n}); err != nil {
			errs = append(errs, fmt.Errorf("delete network %s: %w", n, err))
		}
	}
	if err := m.state.Delete(name, uuid); err != nil {
		errs = append(errs, fmt.Errorf("delete instance record: %w", err))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// List returns every installed instance across all plugins.
func (m *Manager) List(ctx context.Context) ([]Instance, error) {
	_ = ctx
	return m.state.List()
}

// rollback tears down whatever was created up to the failure point.
// Used internally by Install — best-effort, returns the original err.
func (m *Manager) rollback(ctx context.Context, inst Instance, cause error) error {
	for _, v := range inst.VMs {
		_, _ = m.client.DeleteVM(ctx, &weftv1.DeleteVMRequest{Name: v, Project: inst.Project})
	}
	for _, vol := range inst.Volumes {
		_, _ = m.client.DeleteVolume(ctx, &weftv1.DeleteVolumeRequest{Uuid: vol})
	}
	for _, sg := range inst.SecurityGroups {
		_, _ = m.client.DeleteSecurityGroup(ctx, &weftv1.DeleteSecurityGroupRequest{Uuid: sg})
	}
	for _, n := range inst.Networks {
		_, _ = m.client.DeleteNetwork(ctx, &weftv1.DeleteNetworkRequest{Uuid: n})
	}
	return cause
}

// deriveSchedulingRule fabricates a per-VM scheduling rule name
// from the plugin instance and VMSpec. The actual placement
// constraints (az = "different", rack = "different", …) are owned
// by the scheduler ; the rule name is the binding handle weft-
// network watches for. Per the nominal-binding memo, this name
// counts the VM under the rule even if its properties don't match.
func (m *Manager) deriveSchedulingRule(_ *Manifest, vm VMSpec, instanceUUID string) string {
	return fmt.Sprintf("plugin-%s-%s", shortUUID(instanceUUID), vm.Name)
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

// DeterministicInstanceUUID hashes (plugin name, project, resolved
// inputs) to a stable UUID-shaped string. Same inputs → same UUID →
// idempotent install.
func DeterministicInstanceUUID(name, project string, inputs map[string]string) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(project))
	h.Write([]byte{0})
	// Sort keys for deterministic order across runs.
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(inputs[k]))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	out := hex.EncodeToString(sum[:16])
	// Format as 8-4-4-4-12 to read like a UUID.
	return fmt.Sprintf("%s-%s-%s-%s-%s", out[0:8], out[8:12], out[12:16], out[16:20], out[20:32])
}

func qualifiedName(plugin, uuid, name string) string {
	return fmt.Sprintf("%s-%s-%s", plugin, shortUUID(uuid), name)
}

func replicaName(plugin, uuid, vm string, index int) string {
	return fmt.Sprintf("%s-%s-%s-%d", plugin, shortUUID(uuid), vm, index)
}

func shortUUID(u string) string {
	if len(u) >= 8 {
		return u[:8]
	}
	return u
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func maskSecrets(m *Manifest, in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	secrets := map[string]struct{}{}
	for _, inp := range m.Inputs {
		if inp.Secret {
			secrets[inp.Name] = struct{}{}
		}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if _, ok := secrets[k]; ok {
			out[k] = "***"
			continue
		}
		out[k] = v
	}
	return out
}
