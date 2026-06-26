package main

// plugin_server.go owns the server-side Plugin RPCs. The catalogue
// (`catalogue/<name>/plugin.hcl`) is loaded via
// `pluginstore.LoadCatalogue` ; the installed-instance registry +
// install pipeline both live in `pluginstore.Manager`.
//
// The Install RPC dispatches into the same Manager the CLI's
// `weft plugin install` uses — single source of truth for the
// network/SG/VM expansion ordering.

import (
	"context"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/pluginstore"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pluginManager is the narrow seam the server exercises against the
// pluginstore.Manager + LoadCatalogue surface. Tests substitute a fake
// to drive list-empty / list-after-install / unknown-name without
// spinning a Manager + agent client.
type pluginManager interface {
	LoadCatalogue() (map[string]*pluginstore.Manifest, error)
	ListInstalled() ([]pluginstore.Instance, error)
	Install(ctx context.Context, name, project string, inputs map[string]any) (pluginstore.Instance, error)
}

// realPluginManager wires pluginstore.LoadCatalogue + a pluginstore.Manager
// to the pluginManager interface. catalogueDir + manager are set at
// startup ; either may be empty/nil when the agent is launched without
// the plugin surface (tests, minimal dev runs).
//
// etcdCli, when non-nil, overrides the on-disk catalogue path : the
// agent reads /weft/catalogue/* (cf. pluginstore.EtcdCataloguePrefix)
// instead of catalogueDir. Matches the [openweft etcd embedded]
// directive — cluster state lives in etcd so all 6 hosts in the
// 3-DC fleet see the same catalogue without per-host rsync. The
// disk path stays available as a fallback so single-host dev / tests
// keep working.
type realPluginManager struct {
	catalogueDir string
	manager      *pluginstore.Manager
	state        pluginstore.StateStore
	etcdCli      *clientv3.Client
}

func (r *realPluginManager) LoadCatalogue() (map[string]*pluginstore.Manifest, error) {
	// Prefer etcd when available : the cluster's source of truth.
	// On etcd error / empty result, fall back to disk so a temporary
	// etcd hiccup doesn't blank the operator's catalogue view.
	if r.etcdCli != nil {
		cat, err := pluginstore.LoadCatalogueFromEtcd(context.Background(), r.etcdCli, "")
		if err == nil && len(cat) > 0 {
			return cat, nil
		}
		// fall through to disk
	}
	if r.catalogueDir == "" {
		return map[string]*pluginstore.Manifest{}, nil
	}
	return pluginstore.LoadCatalogue(r.catalogueDir)
}

func (r *realPluginManager) ListInstalled() ([]pluginstore.Instance, error) {
	if r.state == nil {
		return nil, nil
	}
	return r.state.List()
}

func (r *realPluginManager) Install(ctx context.Context, name, project string, inputs map[string]any) (pluginstore.Instance, error) {
	cat, err := r.LoadCatalogue()
	if err != nil {
		return pluginstore.Instance{}, err
	}
	m, ok := cat[name]
	if !ok {
		return pluginstore.Instance{}, status.Errorf(codes.NotFound, "plugin %q not found in catalogue", name)
	}
	if r.manager == nil {
		return pluginstore.Instance{}, status.Error(codes.Unavailable, "plugin manager not configured")
	}
	return r.manager.Install(ctx, m, project, inputs)
}

func (s *weftServer) ListPluginCatalogue(_ context.Context, _ *weftv1.ListPluginCatalogueRequest) (*weftv1.ListPluginCatalogueResponse, error) {
	if s.plugins == nil {
		return &weftv1.ListPluginCatalogueResponse{}, nil
	}
	cat, err := s.plugins.LoadCatalogue()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load catalogue: %v", err)
	}
	out := &weftv1.ListPluginCatalogueResponse{
		Entries: make([]*weftv1.PluginCatalogueEntry, 0, len(cat)),
	}
	for _, m := range cat {
		entry := &weftv1.PluginCatalogueEntry{
			Name:        m.Name,
			Version:     m.Version,
			Kind:        m.Kind,
			Description: m.Description,
			Inputs:      make([]*weftv1.PluginInput, 0, len(m.Inputs)),
		}
		for _, in := range m.Inputs {
			ty := in.Type
			if ty == "" {
				ty = "string"
			}
			entry.Inputs = append(entry.Inputs, &weftv1.PluginInput{
				Name:     in.Name,
				Type:     ty,
				Default:  in.Default,
				Required: in.Required,
				Secret:   in.Secret,
				Help:     in.Help,
			})
		}
		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}

func (s *weftServer) ListInstalledPlugins(_ context.Context, _ *weftv1.ListInstalledPluginsRequest) (*weftv1.ListInstalledPluginsResponse, error) {
	if s.plugins == nil {
		return &weftv1.ListInstalledPluginsResponse{}, nil
	}
	items, err := s.plugins.ListInstalled()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list installed plugins: %v", err)
	}
	out := &weftv1.ListInstalledPluginsResponse{
		Instances: make([]*weftv1.PluginInstance, 0, len(items)),
	}
	for _, i := range items {
		row := &weftv1.PluginInstance{
			Name:            i.Name,
			InstanceUuid:    i.UUID,
			Project:         i.Project,
			VmUuids:         append([]string(nil), i.VMs...),
			Status:          "running",
		}
		if !i.InstalledAt.IsZero() {
			row.InstalledAtUnixNs = i.InstalledAt.UnixNano()
		}
		out.Instances = append(out.Instances, row)
	}
	return out, nil
}

func (s *weftServer) InstallPlugin(ctx context.Context, req *weftv1.InstallPluginRequest) (*weftv1.InstallPluginResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.Project == "" {
		return nil, status.Error(codes.InvalidArgument, "project is required")
	}
	if s.plugins == nil {
		return nil, status.Error(codes.Unavailable, "plugin manager not configured")
	}
	// Promote map[string]string → map[string]any since pluginstore.Manager
	// validates against `any` (resolved inputs go through cty / strconv
	// inside ValidateInputs).
	inputs := make(map[string]any, len(req.Inputs))
	for k, v := range req.Inputs {
		inputs[k] = v
	}
	inst, err := s.plugins.Install(ctx, req.Name, req.Project, inputs)
	if err != nil {
		if st, ok := status.FromError(err); ok {
			return nil, st.Err()
		}
		return nil, status.Errorf(codes.Internal, "install: %v", err)
	}
	return &weftv1.InstallPluginResponse{InstanceUuid: inst.UUID}, nil
}
