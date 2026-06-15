package main

// networks.go implements the five Network RPCs on top of the
// Adapter's network registry wrappers. Same ACL discipline as
// volumes.go: ListNetworks scoped to VisibleProjects; mutations
// resolve the network's owning project before authorising the
// caller.

import (
	"context"

	"github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func toNetworkInfo(n weft.Network) *weftv1.NetworkInfo {
	return &weftv1.NetworkInfo{
		Uuid:                      n.UUID,
		ProjectUuid:               n.ProjectUUID,
		Name:                      n.Name,
		Cidr:                      n.CIDR,
		Gateway:                   n.Gateway,
		DnsServers:                n.DNSServers,
		Type:                      string(n.Type),
		CreatedAtUnixNs:           n.CreatedAt.UnixNano(),
		DefaultSecurityGroupUuids: n.DefaultSecurityGroups,
		ExternalMode:              n.ExternalMode,
		Vlan:                      int32(n.VLAN),
		ParentInterface:           n.ParentInterface,
	}
}

func toPortInfo(p weft.Port) *weftv1.PortInfo {
	return &weftv1.PortInfo{
		Uuid:            p.UUID,
		ProjectUuid:     p.ProjectUUID,
		VmUuid:          p.VMUUID,
		NetworkUuid:     p.NetworkUUID,
		Mac:             p.MAC,
		Ip:              p.IP,
		SecurityGroups:  p.SecurityGroups,
		WireguardPubKey: p.WireguardPubKey,
		MeshEndpoint:    p.MeshEndpoint,
		CreatedAtUnixNs: p.CreatedAt.UnixNano(),
		// IngressMbps / EgressMbps not yet persisted on Port — left
		// at zero until portqos persistence lands.
	}
}

// ListPortsForVM resolves the VM via uuid OR (name + project) then
// returns every NIC binding. Read-only ; the operator inspects
// MAC/IP/security-groups + (later) QoS caps. RBAC scopes via
// AuthorizeProject on the owning project.
func (s *weftServer) ListPortsForVM(ctx context.Context, req *weftv1.ListPortsForVMRequest) (*weftv1.ListPortsForVMResponse, error) {
	var vm weft.VM
	var ok bool
	if req.VmUuid != "" {
		vm, ok = s.adp.VMByUUID(req.VmUuid)
	} else if req.VmName != "" {
		projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		vm, ok = s.adp.VMByName(projUUID, req.VmName)
	} else {
		return nil, status.Error(codes.InvalidArgument, "vm_uuid or (vm_name + project) is required")
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "vm not found")
	}
	if _, err := s.adp.AuthorizeProject(ctx, vm.ProjectUUID); err != nil {
		return nil, err
	}
	ports := s.adp.ListPortsForVM(vm.UUID)
	out := make([]*weftv1.PortInfo, len(ports))
	for i, p := range ports {
		out[i] = toPortInfo(p)
	}
	return &weftv1.ListPortsForVMResponse{Ports: out}, nil
}

func (s *weftServer) ListNetworks(ctx context.Context, req *weftv1.ListNetworksRequest) (*weftv1.ListNetworksResponse, error) {
	visible, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return nil, err
	}
	var wantProjectUUID string
	if req.Project != "" {
		uuid, err := s.adp.AuthorizeProject(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		wantProjectUUID = uuid
	}
	out := []*weftv1.NetworkInfo{}
	for _, n := range s.adp.Networks() {
		if wantProjectUUID != "" && n.ProjectUUID != wantProjectUUID {
			continue
		}
		if !all {
			if _, ok := visible[n.ProjectUUID]; !ok {
				continue
			}
		}
		out = append(out, toNetworkInfo(n))
	}
	return &weftv1.ListNetworksResponse{Networks: out}, nil
}

func (s *weftServer) CreateNetwork(ctx context.Context, req *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.Cidr == "" {
		return nil, status.Error(codes.InvalidArgument, "cidr is required")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	netType := weft.NetworkType(req.Type)
	if netType == "" {
		netType = weft.NetworkTypeNAT
	}
	n, err := s.adp.CreateNetwork(weft.CreateNetworkSpec{
		ProjectUUID:     projUUID,
		Name:            req.Name,
		CIDR:            req.Cidr,
		Gateway:         req.Gateway,
		DNSServers:      req.DnsServers,
		Type:            netType,
		ExternalMode:    req.ExternalMode,
		VLAN:            int(req.Vlan),
		ParentInterface: req.ParentInterface,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create network: %v", err)
	}
	logger.Printf("CreateNetwork name=%s project=%s uuid=%s cidr=%s", n.Name, n.ProjectUUID, n.UUID, n.CIDR)
	return &weftv1.CreateNetworkResponse{Network: toNetworkInfo(n)}, nil
}

// authNetwork mirrors authVolume: resolve by UUID, check the
// caller's project access. Hides existence from cross-project
// peeking by returning PermissionDenied for both unknown and
// unauthorised cases.
func (s *weftServer) authNetwork(ctx context.Context, uuid string) (weft.Network, error) {
	if uuid == "" {
		return weft.Network{}, status.Error(codes.InvalidArgument, "uuid is required")
	}
	n, ok := s.adp.NetworkByUUID(uuid)
	if !ok {
		return weft.Network{}, status.Errorf(codes.PermissionDenied, "no access to network %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, n.ProjectUUID); err != nil {
		return weft.Network{}, err
	}
	return n, nil
}

func (s *weftServer) RenameNetwork(ctx context.Context, req *weftv1.RenameNetworkRequest) (*weftv1.RenameNetworkResponse, error) {
	if req.NewName == "" {
		return nil, status.Error(codes.InvalidArgument, "new_name is required")
	}
	if _, err := s.authNetwork(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.RenameNetwork(req.Uuid, req.NewName); err != nil {
		return nil, status.Errorf(codes.Internal, "rename network: %v", err)
	}
	n, _ := s.adp.NetworkByUUID(req.Uuid)
	return &weftv1.RenameNetworkResponse{Network: toNetworkInfo(n)}, nil
}

func (s *weftServer) SetNetworkDNS(ctx context.Context, req *weftv1.SetNetworkDNSRequest) (*weftv1.SetNetworkDNSResponse, error) {
	if _, err := s.authNetwork(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.SetNetworkDNS(req.Uuid, req.DnsServers); err != nil {
		return nil, status.Errorf(codes.Internal, "set dns: %v", err)
	}
	n, _ := s.adp.NetworkByUUID(req.Uuid)
	return &weftv1.SetNetworkDNSResponse{Network: toNetworkInfo(n)}, nil
}

// SetNetworkDefaultSecurityGroups replaces the network's default
// SG list atomically. Validation that each SG exists inside the
// same project as the network is done registry-side
// (setDefaultSecurityGroups) so we don't have to re-implement it
// at the wire boundary.
func (s *weftServer) SetNetworkDefaultSecurityGroups(ctx context.Context, req *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error) {
	if _, err := s.authNetwork(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.SetNetworkDefaultSecurityGroups(req.Uuid, req.SecurityGroupUuids); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "set default security groups: %v", err)
	}
	n, _ := s.adp.NetworkByUUID(req.Uuid)
	logger.Printf("SetNetworkDefaultSecurityGroups uuid=%s count=%d", req.Uuid, len(req.SecurityGroupUuids))
	return &weftv1.SetNetworkDefaultSecurityGroupsResponse{Network: toNetworkInfo(n)}, nil
}

func (s *weftServer) DeleteNetwork(ctx context.Context, req *weftv1.DeleteNetworkRequest) (*weftv1.DeleteNetworkResponse, error) {
	if _, err := s.authNetwork(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteNetwork(req.Uuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "delete network: %v", err)
	}
	logger.Printf("DeleteNetwork uuid=%s", req.Uuid)
	return &weftv1.DeleteNetworkResponse{}, nil
}
